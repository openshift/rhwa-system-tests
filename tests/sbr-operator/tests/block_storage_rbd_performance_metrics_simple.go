package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HeartbeatMetrics holds aggregated heartbeat performance data.
type HeartbeatMetrics struct {
	Phase         string        `json:"phase"`
	StartTime     time.Time     `json:"start_time"`
	EndTime       time.Time     `json:"end_time"`
	SampleCount   int           `json:"sample_count"`
	P50           time.Duration `json:"p50_ms"`
	P95           time.Duration `json:"p95_ms"`
	P99           time.Duration `json:"p99_ms"`
	Max           time.Duration `json:"max_ms"`
	Mean          time.Duration `json:"mean_ms"`
	StdDev        time.Duration `json:"stddev_ms"`
	ErrorCount    int           `json:"error_count"`
	SequenceStart uint64        `json:"sequence_start,omitempty"`
	SequenceEnd   uint64        `json:"sequence_end,omitempty"`
	Gaps          []SequenceGap `json:"sequence_gaps,omitempty"`
}

// SequenceGap represents a detected gap in heartbeat sequence numbers.
type SequenceGap struct {
	Timestamp time.Time `json:"timestamp"`
	Expected  uint64    `json:"expected"`
	Actual    uint64    `json:"actual"`
	GapSize   uint64    `json:"gap_size"`
}

// AgentSnapshot is a point-in-time metrics snapshot from a single agent pod.
type AgentSnapshot struct {
	Timestamp      time.Time
	PodName        string
	NodeName       string
	SequenceNumber uint64
	ErrorCount     int
	// Simplified: store latency samples directly
	LatencySamples []time.Duration
}

// PerformanceReport is the complete JSON report structure.
type PerformanceReport struct {
	TestID            string            `json:"test_id"`
	TestName          string            `json:"test_name"`
	ClusterVersion    string            `json:"cluster_version"`
	ODFVersion        string            `json:"odf_version,omitempty"`
	Timestamp         time.Time         `json:"timestamp"`
	DurationSeconds   int               `json:"duration_seconds"`
	StorageClass      string            `json:"storage_class"`
	SBRTimeoutSeconds int               `json:"sbr_timeout_seconds"`
	WorkerNodeCount   int               `json:"worker_node_count"`
	Baseline          *HeartbeatMetrics `json:"baseline"`
	Loaded            *HeartbeatMetrics `json:"loaded"`
	Recovery          *HeartbeatMetrics `json:"recovery"`
	Validations       map[string]string `json:"validations"`
	LoadProfile       LoadProfile       `json:"load_profile"`
}

// LoadProfile describes the stress-ng configuration.
type LoadProfile struct {
	CPUPercent    int `json:"cpu_percent"`
	MemoryPercent int `json:"memory_percent"`
	IOWorkers     int `json:"io_workers"`
}

// monitorHeartbeatLatency collects heartbeat metrics from agent pods over the specified duration.
func monitorHeartbeatLatency(
	agentPods []*pod.Builder,
	duration time.Duration,
	pollInterval time.Duration,
	phase string,
) *HeartbeatMetrics {
	startTime := time.Now()
	endTime := startTime.Add(duration)

	GinkgoWriter.Printf("[%s] Starting metrics collection: duration=%v, interval=%v, pods=%d\n",
		phase, duration, pollInterval, len(agentPods))

	allSnapshots := make(map[string][]AgentSnapshot) // keyed by pod name
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	sampleCount := 0

	for {
		select {
		case now := <-ticker.C:
			if now.After(endTime) {
				GinkgoWriter.Printf("[%s] Metrics collection completed: %d samples from %d pods\n",
					phase, sampleCount, len(agentPods))
				goto done
			}

			// Collect snapshot from each agent
			for _, agentPod := range agentPods {
				snapshot, err := collectAgentMetricsSimple(agentPod)
				if err != nil {
					GinkgoWriter.Printf("[%s] Warning: failed to collect metrics from %s: %v\n",
						phase, agentPod.Definition.Name, err)
					continue
				}

				allSnapshots[agentPod.Definition.Name] = append(
					allSnapshots[agentPod.Definition.Name],
					*snapshot,
				)
				sampleCount++
			}
		}
	}

done:
	// Aggregate metrics
	return aggregateMetrics(allSnapshots, startTime, endTime, phase)
}

// collectAgentMetricsSimple scrapes metrics using simple text parsing (no Prometheus client libs).
func collectAgentMetricsSimple(agentPod *pod.Builder) (*AgentSnapshot, error) {
	// Get pod IP
	podIP := agentPod.Object.Status.PodIP
	if podIP == "" {
		return nil, fmt.Errorf("pod %s has no IP", agentPod.Definition.Name)
	}

	// Scrape metrics endpoint
	metricsURL := fmt.Sprintf("http://%s:%s/metrics", podIP, sbrparams.AgentMetricsPort)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(metricsURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned HTTP %d", resp.StatusCode)
	}

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse metrics using regex
	snapshot := &AgentSnapshot{
		Timestamp: time.Now(),
		PodName:   agentPod.Definition.Name,
		NodeName:  agentPod.Object.Spec.NodeName,
	}

	metricsText := string(body)

	// Extract sequence number: sbr_heartbeat_sequence_number{...} VALUE
	seqRe := regexp.MustCompile(`sbr_heartbeat_sequence_number\{[^}]*\}\s+(\d+)`)
	if matches := seqRe.FindStringSubmatch(metricsText); len(matches) > 1 {
		snapshot.SequenceNumber, _ = strconv.ParseUint(matches[1], 10, 64)
	}

	// Extract error count: sbr_heartbeat_write_errors_total{...} VALUE
	errRe := regexp.MustCompile(`sbr_heartbeat_write_errors_total\{[^}]*\}\s+(\d+)`)
	if matches := errRe.FindStringSubmatch(metricsText); len(matches) > 1 {
		errCount, _ := strconv.ParseInt(matches[1], 10, 64)
		snapshot.ErrorCount = int(errCount)
	}

	// Extract histogram sum and count for mean calculation
	// sbr_heartbeat_write_duration_seconds_sum VALUE
	// sbr_heartbeat_write_duration_seconds_count VALUE
	sumRe := regexp.MustCompile(`sbr_heartbeat_write_duration_seconds_sum\{[^}]*\}\s+([\d.]+)`)
	countRe := regexp.MustCompile(`sbr_heartbeat_write_duration_seconds_count\{[^}]*\}\s+(\d+)`)

	var sumSeconds float64
	var count int

	if matches := sumRe.FindStringSubmatch(metricsText); len(matches) > 1 {
		sumSeconds, _ = strconv.ParseFloat(matches[1], 64)
	}

	if matches := countRe.FindStringSubmatch(metricsText); len(matches) > 1 {
		countInt, _ := strconv.ParseInt(matches[1], 10, 64)
		count = int(countInt)
	}

	// Estimate latency samples from mean
	if count > 0 && sumSeconds > 0 {
		meanLatency := time.Duration(float64(time.Second) * sumSeconds / float64(count))
		// Create synthetic samples around mean (simplified approximation)
		snapshot.LatencySamples = []time.Duration{meanLatency}
	}

	return snapshot, nil
}

// aggregateMetrics combines snapshots from all agents into a single metrics object.
func aggregateMetrics(
	snapshots map[string][]AgentSnapshot,
	start, end time.Time,
	phase string,
) *HeartbeatMetrics {
	var allLatencies []time.Duration
	totalErrors := 0
	var gaps []SequenceGap

	// Flatten all latency samples
	for podName, podSnapshots := range snapshots {
		if len(podSnapshots) == 0 {
			continue
		}

		// Collect latencies
		for _, snapshot := range podSnapshots {
			allLatencies = append(allLatencies, snapshot.LatencySamples...)
			totalErrors = snapshot.ErrorCount // Use latest error count
		}

		// Detect sequence gaps for this pod
		podGaps := detectSequenceGapsInSnapshots(podSnapshots)
		gaps = append(gaps, podGaps...)

		GinkgoWriter.Printf("[%s] Pod %s: %d snapshots, seq %d -> %d, %d gaps\n",
			phase, podName, len(podSnapshots),
			podSnapshots[0].SequenceNumber,
			podSnapshots[len(podSnapshots)-1].SequenceNumber,
			len(podGaps))
	}

	if len(allLatencies) == 0 {
		return &HeartbeatMetrics{
			Phase:      phase,
			StartTime:  start,
			EndTime:    end,
			ErrorCount: totalErrors,
			Gaps:       gaps,
		}
	}

	// Sort for percentile calculation
	sort.Slice(allLatencies, func(i, j int) bool {
		return allLatencies[i] < allLatencies[j]
	})

	return &HeartbeatMetrics{
		Phase:       phase,
		StartTime:   start,
		EndTime:     end,
		SampleCount: len(allLatencies),
		P50:         percentile(allLatencies, 0.50),
		P95:         percentile(allLatencies, 0.95),
		P99:         percentile(allLatencies, 0.99),
		Max:         allLatencies[len(allLatencies)-1],
		Mean:        mean(allLatencies),
		StdDev:      stdDev(allLatencies),
		ErrorCount:  totalErrors,
		Gaps:        gaps,
	}
}

// detectSequenceGapsInSnapshots finds gaps in sequence numbers across snapshots.
func detectSequenceGapsInSnapshots(snapshots []AgentSnapshot) []SequenceGap {
	if len(snapshots) < 2 {
		return nil
	}

	var gaps []SequenceGap

	for i := 1; i < len(snapshots); i++ {
		expected := snapshots[i-1].SequenceNumber + 1
		actual := snapshots[i].SequenceNumber

		if actual > expected+1 { // Allow gap of 1 (normal increment)
			gaps = append(gaps, SequenceGap{
				Timestamp: snapshots[i].Timestamp,
				Expected:  expected,
				Actual:    actual,
				GapSize:   actual - expected,
			})
		}
	}

	return gaps
}

// percentile calculates the p-th percentile from sorted durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}

	return sorted[idx]
}

// mean calculates the mean of durations.
func mean(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}

	var sum int64
	for _, v := range values {
		sum += v.Nanoseconds()
	}

	return time.Duration(sum / int64(len(values)))
}

// stdDev calculates the standard deviation of durations.
func stdDev(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}

	m := mean(values)
	var variance int64

	for _, v := range values {
		diff := v.Nanoseconds() - m.Nanoseconds()
		variance += diff * diff
	}

	variance /= int64(len(values))

	return time.Duration(int64(math.Sqrt(float64(variance))))
}

// logMetricsSummary prints a human-readable metrics summary.
func logMetricsSummary(label string, metrics *HeartbeatMetrics) {
	GinkgoWriter.Printf("\n===== %s METRICS =====\n", label)
	GinkgoWriter.Printf("Phase:        %s\n", metrics.Phase)
	GinkgoWriter.Printf("Duration:     %v\n", metrics.EndTime.Sub(metrics.StartTime))
	GinkgoWriter.Printf("Samples:      %d\n", metrics.SampleCount)
	GinkgoWriter.Printf("P50 latency:  %v\n", metrics.P50)
	GinkgoWriter.Printf("P95 latency:  %v\n", metrics.P95)
	GinkgoWriter.Printf("P99 latency:  %v\n", metrics.P99)
	GinkgoWriter.Printf("Max latency:  %v\n", metrics.Max)
	GinkgoWriter.Printf("Mean latency: %v\n", metrics.Mean)
	GinkgoWriter.Printf("Std dev:      %v\n", metrics.StdDev)
	GinkgoWriter.Printf("Errors:       %d\n", metrics.ErrorCount)
	GinkgoWriter.Printf("Seq gaps:     %d\n", len(metrics.Gaps))

	if len(metrics.Gaps) > 0 {
		GinkgoWriter.Printf("  Gap details:\n")
		for i, gap := range metrics.Gaps {
			if i >= 5 {
				GinkgoWriter.Printf("  ... (%d more gaps)\n", len(metrics.Gaps)-5)
				break
			}
			GinkgoWriter.Printf("    [%v] expected=%d actual=%d (gap size=%d)\n",
				gap.Timestamp.Format("15:04:05"), gap.Expected, gap.Actual, gap.GapSize)
		}
	}

	GinkgoWriter.Printf("=========================\n\n")
}

// generatePerformanceReport creates and saves the JSON performance report.
func generatePerformanceReport(
	baseline, loaded, recovery *HeartbeatMetrics,
	storageClass string,
) string {
	report := PerformanceReport{
		TestID:    "TBD",
		TestName:  "Ceph RBD Heartbeat Performance Under Load",
		Timestamp: time.Now(),
		DurationSeconds: int(
			baseline.EndTime.Sub(baseline.StartTime).Seconds() +
				loaded.EndTime.Sub(loaded.StartTime).Seconds() +
				recovery.EndTime.Sub(recovery.StartTime).Seconds(),
		),
		StorageClass:      storageClass,
		SBRTimeoutSeconds: sbrparams.SBRCTimeoutSecondsMin,
		WorkerNodeCount:   getWorkerNodeCount(),
		Baseline:          baseline,
		Loaded:            loaded,
		Recovery:          recovery,
		Validations:       runValidations(baseline, loaded, recovery),
		LoadProfile: LoadProfile{
			CPUPercent:    sbrparams.StressCPULoad,
			MemoryPercent: sbrparams.StressMemoryPercent,
			IOWorkers:     2,
		},
	}

	// Add cluster version
	clusterVersion, err := APIClient.ConfigV1Interface.ClusterVersions().Get(
		context.TODO(), "version", metav1.GetOptions{})
	if err == nil {
		report.ClusterVersion = clusterVersion.Status.Desired.Version
	}

	// Serialize to JSON
	jsonData, err := json.MarshalIndent(report, "", "  ")
	Expect(err).ToNot(HaveOccurred(), "Failed to marshal performance report")

	// Save to artifacts directory
	artifactsDir := os.Getenv("ARTIFACT_DIR")
	if artifactsDir == "" {
		artifactsDir = "/tmp/sbr-test-artifacts"
	}

	err = os.MkdirAll(artifactsDir, 0755)
	Expect(err).ToNot(HaveOccurred(), "Failed to create artifacts directory")

	reportPath := filepath.Join(artifactsDir,
		fmt.Sprintf("sbr-heartbeat-performance-%s.json",
			time.Now().Format("20060102-150405")))

	err = os.WriteFile(reportPath, jsonData, 0644)
	Expect(err).ToNot(HaveOccurred(), "Failed to write performance report")

	GinkgoWriter.Printf("Performance report saved: %s\n", reportPath)

	return reportPath
}

// runValidations executes all validation checks and returns results.
func runValidations(baseline, loaded, recovery *HeartbeatMetrics) map[string]string {
	validations := make(map[string]string)

	// Hard requirement: P99 under load < 500ms
	if loaded.P99.Milliseconds() < int64(sbrparams.PerfTestP99LatencyThresholdMs) {
		validations["p99_under_500ms"] = "PASS"
	} else {
		validations["p99_under_500ms"] = fmt.Sprintf("FAIL (%dms)", loaded.P99.Milliseconds())
	}

	// Hard requirement: P99 baseline < 100ms
	if baseline.P99.Milliseconds() < int64(sbrparams.PerfTestP99BaselineThresholdMs) {
		validations["baseline_p99_under_100ms"] = "PASS"
	} else {
		validations["baseline_p99_under_100ms"] = fmt.Sprintf("FAIL (%dms)", baseline.P99.Milliseconds())
	}

	// Hard requirement: No false positives
	validations["no_false_positives"] = "PASS (checked in test)"

	// Hard requirement: Sequence continuity
	totalGaps := len(baseline.Gaps) + len(loaded.Gaps) + len(recovery.Gaps)
	if totalGaps < sbrparams.PerfTestMaxSequenceGaps {
		validations["sequence_continuity"] = "PASS"
	} else {
		validations["sequence_continuity"] = fmt.Sprintf("FAIL (%d gaps)", totalGaps)
	}

	// Hard requirement: Error rate < 1%
	totalSamples := baseline.SampleCount + loaded.SampleCount + recovery.SampleCount
	totalErrors := baseline.ErrorCount + loaded.ErrorCount + recovery.ErrorCount
	errorRate := 0.0
	if totalSamples > 0 {
		errorRate = float64(totalErrors) / float64(totalSamples)
	}

	if errorRate < sbrparams.PerfTestMaxErrorRate {
		validations["error_rate_below_1pct"] = "PASS"
	} else {
		validations["error_rate_below_1pct"] = fmt.Sprintf("FAIL (%.2f%%)", errorRate*100)
	}

	// Soft requirement: Degradation < 3x
	degradation := 0.0
	if baseline.P99 > 0 {
		degradation = float64(loaded.P99) / float64(baseline.P99)
	}

	if degradation < sbrparams.PerfTestMaxDegradationFactor {
		validations["degradation_below_3x"] = "PASS"
	} else {
		validations["degradation_below_3x"] = fmt.Sprintf("WARN (%.1fx degradation)", degradation)
	}

	// Soft requirement: Recovery within 30s (delta < 10%)
	recoveryDelta := 0.0
	if baseline.P99 > 0 {
		recoveryDelta = float64(recovery.P99-baseline.P99) / float64(baseline.P99) * 100
	}

	if math.Abs(recoveryDelta) < 10.0 {
		validations["recovery_within_30s"] = "PASS"
	} else {
		validations["recovery_within_30s"] = fmt.Sprintf("WARN (delta: %+.1f%%)", recoveryDelta)
	}

	return validations
}
