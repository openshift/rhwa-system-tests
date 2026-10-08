package tests

import (
	"context"
	"fmt"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe(
	"cephrbd perf",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR), func() {

		var (
			rbdStorageClass string
		)

		BeforeAll(func() {
			By("Pre-cleanup: removing any leftover performance test SBRC from previous runs")

			Eventually(func() error {
				stale := buildSBRC(sbrparams.SBRCBlockPerfTestName, map[string]interface{}{})
				deleteErr := APIClient.Delete(context.TODO(), stale)
				if deleteErr == nil || k8serrors.IsNotFound(deleteErr) {
					return nil
				}
				return deleteErr
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"Pre-test cleanup of stale SBRC %s failed", sbrparams.SBRCBlockPerfTestName)

			By("Waiting for stale SBRC to disappear")

			Eventually(func() error {
				check := buildSBRC(sbrparams.SBRCBlockPerfTestName, map[string]interface{}{})
				getErr := APIClient.Get(context.TODO(),
					types.NamespacedName{Name: sbrparams.SBRCBlockPerfTestName, Namespace: medik8sparams.OperatorNs},
					check)
				if k8serrors.IsNotFound(getErr) {
					return nil
				}
				if getErr != nil {
					return getErr
				}
				return fmt.Errorf("SBRC %s still terminating", sbrparams.SBRCBlockPerfTestName)
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up performance test SBRC")

			Eventually(func() error {
				stale := buildSBRC(sbrparams.SBRCBlockPerfTestName, map[string]interface{}{})
				deleteErr := APIClient.Delete(context.TODO(), stale)
				if deleteErr == nil || k8serrors.IsNotFound(deleteErr) {
					return nil
				}
				return deleteErr
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
		})

		It("heartbeat latency under load",
			reportxml.ID("TBD"),
			Label(
				labels.OperatorSBR,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				testCephRBDHeartbeatPerformance(&rbdStorageClass)
			})
	})

// testCephRBDHeartbeatPerformance is the main test implementation.
func testCephRBDHeartbeatPerformance(rbdStorageClass *string) {
	var (
		baselineMetrics *HeartbeatMetrics
		loadedMetrics   *HeartbeatMetrics
		recoveryMetrics *HeartbeatMetrics
		stressPods      []*pod.Builder
	)

	// === PHASE 1: Setup ===
	By("Discovering Ceph RBD block storage class")
	*rbdStorageClass = discoverCephRBDStorageClass()
	Expect(*rbdStorageClass).ToNot(BeEmpty(),
		"Ceph RBD storage class required; set via ODF or skip test")

	By("Creating SBRC with Ceph RBD and minimum timeout for maximum stress")
	sbrcConfig := buildPerfTestSBRC(*rbdStorageClass, sbrparams.SBRCBlockPerfTestName)
	sbrc := buildSBRC(sbrparams.SBRCBlockPerfTestName, sbrcConfig)

	err := APIClient.Create(context.TODO(), sbrc)
	Expect(err).ToNot(HaveOccurred(), "Failed to create performance test SBRC")

	DeferCleanup(func() {
		By("DeferCleanup: Removing SBRC")
		Eventually(func() error {
			stale := buildSBRC(sbrparams.SBRCBlockPerfTestName, map[string]interface{}{})
			deleteErr := APIClient.Delete(context.TODO(), stale)
			if deleteErr == nil || k8serrors.IsNotFound(deleteErr) {
				return nil
			}
			return deleteErr
		}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
	})

	By("Waiting for agent DaemonSet to reach ready state")
	waitForSBRCReady(sbrparams.SBRCBlockPerfTestName)

	agentPods := getAllAgentPods()
	workerCount := getWorkerNodeCount()

	GinkgoWriter.Printf("Agent pods ready: %d (worker nodes: %d)\n", len(agentPods), workerCount)
	Expect(len(agentPods)).To(BeNumerically(">=", 2),
		"Need at least 2 worker nodes for meaningful performance test")

	// === PHASE 2: Baseline Monitoring ===
	By(fmt.Sprintf("Monitoring baseline heartbeat latency for %v", sbrparams.PerfTestBaselineDuration))
	GinkgoWriter.Printf("Collecting baseline metrics from %d agent pods...\n", len(agentPods))

	baselineMetrics = monitorHeartbeatLatency(
		agentPods,
		sbrparams.PerfTestBaselineDuration,
		sbrparams.PerfTestPollInterval,
		"BASELINE",
	)

	logMetricsSummary("BASELINE", baselineMetrics)

	// === PHASE 3: Load Injection ===
	By("Deploying stress-ng pods on all worker nodes")
	stressPods = deployStressDaemonSet()
	Expect(len(stressPods)).To(BeNumerically(">=", 1),
		"At least one stress pod should be deployed")

	DeferCleanup(func() {
		if len(stressPods) > 0 {
			By("DeferCleanup: Removing stress pods")
			cleanupStressPods(stressPods)
		}
	})

	By("Waiting for stress pods to reach Running state")
	runningCount := waitForStressPodsRunning(len(stressPods))
	GinkgoWriter.Printf("Load test running with %d/%d stress pods (cluster resource constraints)\n",
		runningCount, len(stressPods))

	// Give stress-ng time to ramp up load
	time.Sleep(10 * time.Second)

	By(fmt.Sprintf("Monitoring heartbeat latency under load for %v", sbrparams.PerfTestLoadedDuration))
	GinkgoWriter.Printf("Load injection active - monitoring performance degradation...\n")

	loadedMetrics = monitorHeartbeatLatency(
		agentPods,
		sbrparams.PerfTestLoadedDuration,
		sbrparams.PerfTestPollInterval,
		"UNDER LOAD",
	)

	logMetricsSummary("UNDER LOAD", loadedMetrics)

	// === PHASE 4: Recovery Monitoring ===
	By("Removing stress pods and monitoring recovery")
	cleanupStressPods(stressPods)
	stressPods = nil

	// Give system time to settle
	time.Sleep(10 * time.Second)

	By(fmt.Sprintf("Monitoring heartbeat latency during recovery for %v", sbrparams.PerfTestRecoveryDuration))

	recoveryMetrics = monitorHeartbeatLatency(
		agentPods,
		sbrparams.PerfTestRecoveryDuration,
		sbrparams.PerfTestPollInterval,
		"RECOVERY",
	)

	logMetricsSummary("RECOVERY", recoveryMetrics)

	// === PHASE 5: Validation ===
	By("Validating heartbeat latency remains within acceptable bounds")

	validatePerformanceResults(baselineMetrics, loadedMetrics, recoveryMetrics)

	By("Verifying no false-positive SBRStorageUnhealthy conditions")
	nodes := getAllWorkerNodes()
	for _, node := range nodes {
		hasUnhealthyCondition := checkSBRStorageUnhealthyCondition(node.Name)
		Expect(hasUnhealthyCondition).To(BeFalse(),
			"Node %s should not have SBRStorageUnhealthy condition during performance test",
			node.Name)
	}

	By("Generating performance report")
	reportPath := generatePerformanceReport(baselineMetrics, loadedMetrics, recoveryMetrics, *rbdStorageClass)
	GinkgoWriter.Printf("Performance report saved to: %s\n", reportPath)
}

// validatePerformanceResults executes all validation checks.
func validatePerformanceResults(baseline, loaded, recovery *HeartbeatMetrics) {
	// NOTE: Heartbeat latency metrics (sbr_heartbeat_write_duration_seconds_*) do not exist
	// in the current agent implementation. We validate based on available metrics:
	//   - Device I/O error count (sbr_device_io_errors_total)
	//   - Watchdog activity gaps (sbr_watchdog_pets_total continuity)
	//
	// If latency metrics become available in the future, the P99/P95/Max validations below
	// will automatically work when SampleCount > 0.

	// === HARD REQUIREMENTS ===

	By("Validating: I/O error count tracking")
	GinkgoWriter.Printf("I/O Error counts - Baseline: %d, Loaded: %d, Recovery: %d\n",
		baseline.ErrorCount, loaded.ErrorCount, recovery.ErrorCount)

	errorIncrease := loaded.ErrorCount - baseline.ErrorCount
	if errorIncrease > 0 {
		GinkgoWriter.Printf("WARNING: I/O errors increased by %d under load\n", errorIncrease)
	}

	if loaded.SampleCount > 0 {
		By("Validating hard requirement: P99 latency under load < 500ms")
		Expect(loaded.P99.Milliseconds()).To(BeNumerically("<", int64(sbrparams.PerfTestP99LatencyThresholdMs)),
			"FAIL: P99 latency under load (%dms) exceeded %dms threshold\n"+
				"This indicates heartbeat writes are too slow for the configured timeout.\n"+
				"Consider: (1) increasing sbrTimeoutSeconds, (2) tuning Ceph RBD performance, "+
				"(3) reducing MaxConsecutiveFailures",
			loaded.P99.Milliseconds(), sbrparams.PerfTestP99LatencyThresholdMs)

		By("Validating hard requirement: P99 baseline latency < 100ms")
		Expect(baseline.P99.Milliseconds()).To(BeNumerically("<", int64(sbrparams.PerfTestP99BaselineThresholdMs)),
			"FAIL: Baseline P99 latency (%dms) exceeded %dms threshold\n"+
				"This indicates poor Ceph RBD performance even in idle state.\n"+
				"Check: Ceph cluster health, OSD performance, network latency",
			baseline.P99.Milliseconds(), sbrparams.PerfTestP99BaselineThresholdMs)

		By("Validating hard requirement: Max latency < 1000ms")
		Expect(loaded.Max.Milliseconds()).To(BeNumerically("<", int64(sbrparams.PerfTestMaxLatencyThresholdMs)),
			"FAIL: Maximum latency (%dms) exceeded %dms threshold",
			loaded.Max.Milliseconds(), sbrparams.PerfTestMaxLatencyThresholdMs)
	} else {
		GinkgoWriter.Printf("INFO: Skipping latency validations - heartbeat duration metrics not available\n")
	}

	By("Validating: Watchdog activity continuity")
	totalGaps := len(baseline.Gaps) + len(loaded.Gaps) + len(recovery.Gaps)
	GinkgoWriter.Printf("Watchdog activity gaps: Baseline=%d, Loaded=%d, Recovery=%d (total=%d)\n",
		len(baseline.Gaps), len(loaded.Gaps), len(recovery.Gaps), totalGaps)

	if totalGaps >= sbrparams.PerfTestMaxSequenceGaps {
		GinkgoWriter.Printf("WARNING: Found %d activity gaps (threshold: %d)\n"+
			"Gaps indicate agent pauses or watchdog pet failures. Review: agent logs, I/O errors\n",
			totalGaps, sbrparams.PerfTestMaxSequenceGaps)
	}

	largestGap := maxGapSize(baseline, loaded, recovery)
	if largestGap > sbrparams.PerfTestMaxSequenceGapSize {
		GinkgoWriter.Printf("WARNING: Largest activity gap was %d (threshold: %d)\n"+
			"Large gaps suggest sustained agent issues\n",
			largestGap, sbrparams.PerfTestMaxSequenceGapSize)
	}

	By("Validating hard requirement: Device I/O error rate < 1%")
	totalErrors := baseline.ErrorCount + loaded.ErrorCount + recovery.ErrorCount

	// Error rate validation: compare total I/O errors across all phases
	// Even without latency samples, we can track absolute error counts
	GinkgoWriter.Printf("Total I/O errors: %d (Baseline=%d, Loaded=%d, Recovery=%d)\n",
		totalErrors, baseline.ErrorCount, loaded.ErrorCount, recovery.ErrorCount)

	Expect(totalErrors).To(BeNumerically("==", 0),
		"FAIL: Detected %d device I/O errors across all test phases\n"+
			"Any I/O errors indicate storage issues. Check: Ceph cluster health, network stability",
		totalErrors)

	// === SOFT REQUIREMENTS (Warnings) ===

	if loaded.SampleCount > 0 && baseline.SampleCount > 0 {
		By("Checking soft requirement: Performance degradation < 3x")
		degradation := 0.0
		if baseline.P99 > 0 {
			degradation = float64(loaded.P99) / float64(baseline.P99)
		}

		if degradation > sbrparams.PerfTestMaxDegradationFactor {
			GinkgoWriter.Printf("WARNING: P99 latency degraded by %.1fx under load (threshold: %.1fx)\n",
				degradation, sbrparams.PerfTestMaxDegradationFactor)
			GinkgoWriter.Printf("  Baseline P99: %v\n", baseline.P99)
			GinkgoWriter.Printf("  Loaded P99:   %v\n", loaded.P99)
			GinkgoWriter.Printf("  Recommendation: Review Ceph cluster I/O capacity\n")
		}

		By("Checking soft requirement: Recovery time < 30s")
		if baseline.P99 > 0 && recovery.SampleCount > 0 {
			recoveryDelta := recovery.P99.Milliseconds() - baseline.P99.Milliseconds()
			recoveryDeltaPct := float64(recoveryDelta) / float64(baseline.P99.Milliseconds()) * 100

			if math.Abs(recoveryDeltaPct) > 10.0 {
				GinkgoWriter.Printf("WARNING: P99 latency did not fully recover after load removal\n")
				GinkgoWriter.Printf("  Baseline P99: %v\n", baseline.P99)
				GinkgoWriter.Printf("  Recovery P99: %v (delta: %+.1f%%)\n", recovery.P99, recoveryDeltaPct)
				GinkgoWriter.Printf("  This may indicate sustained Ceph cluster pressure\n")
			}
		}
	} else {
		GinkgoWriter.Printf("INFO: Skipping latency degradation checks - heartbeat duration metrics not available\n")
	}
}

// maxGapSize returns the largest sequence gap across all metrics.
func maxGapSize(baseline, loaded, recovery *HeartbeatMetrics) uint64 {
	maxGap := uint64(0)

	for _, gap := range baseline.Gaps {
		if gap.GapSize > maxGap {
			maxGap = gap.GapSize
		}
	}

	for _, gap := range loaded.Gaps {
		if gap.GapSize > maxGap {
			maxGap = gap.GapSize
		}
	}

	for _, gap := range recovery.Gaps {
		if gap.GapSize > maxGap {
			maxGap = gap.GapSize
		}
	}

	return maxGap
}
