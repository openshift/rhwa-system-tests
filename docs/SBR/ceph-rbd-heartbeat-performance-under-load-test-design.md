# Ceph RBD Heartbeat Performance Under Load - Automated Test Design

## Document Information
- **Test ID**: TBD (Polarion ID to be assigned)
- **Test Category**: SBR Block Storage Performance
- **Priority**: Medium
- **Disruption Level**: Non-destructive
- **Estimated Duration**: 8-12 minutes
- **Target Platform**: OpenShift with ODF (Ceph RBD)
- **Author**: System Tests Team
- **Date Created**: 2026-10-07
- **Related Analysis**: [SBR Test Analysis - Section 4](file:///home/whayutin/RHWA/CI/SBR/sbr_test_analysis.md#4-ceph-rbd-heartbeat-performance-under-load)

---

## Table of Contents
1. [Overview](#overview)
2. [Test Objectives](#test-objectives)
3. [Background and Motivation](#background-and-motivation)
4. [Test Architecture](#test-architecture)
5. [Detailed Test Implementation](#detailed-test-implementation)
6. [Heartbeat Monitoring Strategy](#heartbeat-monitoring-strategy)
7. [Load Generation Patterns](#load-generation-patterns)
8. [Validation Criteria](#validation-criteria)
9. [Data Collection and Metrics](#data-collection-and-metrics)
10. [Integration with CI/CD](#integration-with-cicd)
11. [Expected Outcomes](#expected-outcomes)
12. [Failure Scenarios and Debugging](#failure-scenarios-and-debugging)
13. [Future Enhancements](#future-enhancements)

---

## Overview

This document specifies the design for an automated end-to-end test that validates Storage-Based Remediation (SBR) heartbeat mechanism performance and reliability on Ceph RBD block storage under sustained system load. The test ensures that the SBR agent's heartbeat writes to block devices maintain acceptable latency characteristics even when the cluster experiences CPU, memory, and I/O pressure, preventing false-positive fencing decisions.

### Test Summary

**Purpose**: Validate that SBR heartbeat writes to Ceph RBD block storage remain stable and within acceptable latency bounds under various system load conditions, ensuring the heartbeat timeout mechanism does not trigger false-positive node fencing.

**Scope**: 
- Ceph RBD block storage (volumeMode: Block) with ODF
- Multi-agent concurrent heartbeat monitoring
- System load injection (CPU, memory, I/O)
- Latency measurement and statistical analysis
- Node condition monitoring for false positives

**Key Metrics**:
- Heartbeat write latency (p50, p95, p99, max)
- Sequence number continuity
- SBRStorageUnhealthy condition stability
- CPU/memory usage correlation

---

## Test Objectives

### Primary Objectives

1. **Baseline Performance Establishment**
   - Measure heartbeat write latency in idle cluster state
   - Establish p50/p95/p99 latency baselines for Ceph RBD
   - Document normal heartbeat interval behavior

2. **Load Resistance Validation**
   - Verify heartbeat mechanism remains functional under CPU load
   - Validate latency stays within acceptable bounds under memory pressure
   - Confirm I/O contention doesn't cause heartbeat failures

3. **False-Positive Prevention**
   - Ensure no SBRStorageUnhealthy conditions under normal load
   - Verify no unintended fencing triggers
   - Validate timeout buffer adequacy

4. **Performance Characterization**
   - Quantify Ceph RBD block storage heartbeat latency distribution
   - Identify latency degradation patterns under load
   - Establish safe timeout values for production use

### Secondary Objectives

1. **Multi-Agent Consistency**
   - Verify all agents experience similar latency patterns
   - Detect node-specific performance issues
   - Validate peer heartbeat monitoring accuracy

2. **Storage Backend Behavior**
   - Characterize Ceph RBD CSI driver performance under load
   - Identify storage-layer bottlenecks
   - Document ODF-specific performance characteristics

3. **Regression Detection**
   - Establish performance baselines for CI comparison
   - Enable detection of performance degradation across releases
   - Track heartbeat mechanism optimization over time

---

## Background and Motivation

### Why This Test Is Critical

The SBR heartbeat mechanism is the foundation of storage-based fencing. Each agent writes a heartbeat with a sequence number and timestamp to its slot on the shared block device at regular intervals (typically every `timeout/3` seconds). Other agents monitor these heartbeats to detect storage-isolated or failed nodes.

**Critical Timing Requirement**: If heartbeat writes become too slow due to storage latency, CPU starvation, or I/O contention, the agent may exceed its timeout window, causing peer agents to incorrectly mark the node as storage-unhealthy and trigger fencing - **even though the node is actually healthy**.

### Current Coverage Gap

The existing rhwa-system-tests suite includes:
- ✅ Functional tests for heartbeat writes (CephFS filesystem mode)
- ✅ Storage disruption tests (simulated total I/O loss)
- ✅ Transient failure recovery tests
- ❌ **No performance/latency tests for block storage heartbeats**
- ❌ **No load-injection stress tests**
- ❌ **No Ceph RBD-specific latency characterization**

### Real-World Scenario

In production OpenShift clusters with ODF:
- Nodes may experience CPU pressure from workload bursts
- Memory pressure can cause kernel page reclaim activity
- I/O subsystem contention from other pods
- Ceph RBD latency varies with cluster load

**Without this test**, we cannot confidently answer:
- "What is the p99 heartbeat latency on Ceph RBD under normal load?"
- "Will a 10-second timeout prevent false positives during CPU bursts?"
- "How much latency degradation should we expect under I/O pressure?"

### Block vs Filesystem Mode Differences

Block mode (Ceph RBD) has different performance characteristics than filesystem mode (CephFS):
- **Direct block I/O**: No filesystem layer overhead
- **SCSI commands**: Different I/O path through kernel
- **Ceph librbd**: Different client library than CephFS
- **RBD image locking**: Multi-writer coordination via Ceph
- **Kernel caching**: Block device cache vs page cache

These differences mean CephFS performance data **does not** predict Ceph RBD behavior.

---

## Test Architecture

### Component Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                         Test Orchestrator                        │
│  (Ginkgo Test: testCephRBDHeartbeatPerformance)                 │
└────────────┬────────────────────────────────────────────────────┘
             │
             ├─── Phase 1: Setup
             │    ├─ Discover Ceph RBD StorageClass
             │    ├─ Create SBRC with minimum timeout (10s)
             │    ├─ Wait for agent DaemonSet ready
             │    └─ Select monitoring target nodes
             │
             ├─── Phase 2: Baseline Monitoring (2 min)
             │    ├─ Poll agent pods for heartbeat metrics
             │    ├─ Calculate latency distribution (p50/p95/p99)
             │    └─ Record baseline statistics
             │
             ├─── Phase 3: Load Injection (5 min)
             │    ├─ Deploy stress-ng pods on all workers
             │    │   ├─ CPU stressor (50% load per node)
             │    │   ├─ Memory stressor (30% RAM)
             │    │   └─ I/O stressor (disk churn)
             │    ├─ Monitor heartbeat metrics during load
             │    └─ Calculate loaded latency distribution
             │
             ├─── Phase 4: Recovery Monitoring (2 min)
             │    ├─ Remove stress pods
             │    ├─ Monitor heartbeat return to baseline
             │    └─ Validate recovery time
             │
             └─── Phase 5: Validation
                  ├─ Verify no SBRStorageUnhealthy conditions
                  ├─ Check sequence number continuity
                  ├─ Validate latency bounds (p99 < 500ms)
                  └─ Generate performance report
```

### Key Components

#### 1. SBRC Configuration
```yaml
apiVersion: storage-based-remediation.medik8s.io/v1alpha1
kind: StorageBasedRemediationConfig
metadata:
  name: test-sbr-block-perf
  namespace: openshift-storage-based-remediation
spec:
  sharedStorageClass: ocs-storagecluster-ceph-rbd
  sharedStorageVolumeMode: Block
  sbrTimeoutSeconds: 10  # Minimum timeout for maximum stress
  maxConsecutiveFailures: 3
```

**Rationale for minimum timeout**: Using the minimum allowed timeout (10 seconds) creates the most challenging scenario for heartbeat performance. This provides the least amount of latency headroom and maximizes the likelihood of detecting performance issues.

#### 2. Heartbeat Metrics Collection

The SBR agent exposes Prometheus metrics including:
- `sbr_heartbeat_write_duration_seconds` (histogram)
- `sbr_heartbeat_write_errors_total` (counter)
- `sbr_heartbeat_sequence_number` (gauge)
- `sbr_device_write_latency_seconds` (histogram)

**Collection Method**:
```go
// Poll metrics endpoint from each agent pod
metricsURL := fmt.Sprintf("http://%s:8080/metrics", agentPodIP)
response, err := http.Get(metricsURL)
// Parse Prometheus text format
// Extract histogram buckets and calculate percentiles
```

#### 3. Load Generation

**stress-ng DaemonSet** deployed to all worker nodes:
```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: sbr-perf-stressor
spec:
  selector:
    matchLabels:
      app: sbr-perf-stressor
  template:
    spec:
      nodeSelector:
        node-role.kubernetes.io/worker: ""
      containers:
      - name: stress-ng
        image: registry.fedoraproject.org/fedora:latest
        command:
        - /bin/bash
        - -c
        - |
          dnf install -y stress-ng
          stress-ng \
            --cpu 4 --cpu-load 50 \
            --vm 2 --vm-bytes 30% \
            --hdd 2 --hdd-bytes 1G \
            --timeout 300s \
            --metrics-brief
        resources:
          limits:
            cpu: "4"
            memory: "8Gi"
        securityContext:
          privileged: false
```

**Load Profile**:
- **CPU**: 50% load across 4 cores (moderate sustained pressure)
- **Memory**: 30% of available RAM (forces page cache eviction)
- **I/O**: 2 workers churning 1GB files (creates I/O contention)
- **Duration**: 5 minutes (long enough to observe steady-state behavior)

---

## Detailed Test Implementation

### File Structure

**New Test File**: `tests/sbr-operator/tests/block_storage_rbd_performance.go`

**Helper File**: `tests/sbr-operator/tests/block_storage_rbd_helpers.go` (shared with other RBD tests)

**Constants File**: `tests/sbr-operator/internal/sbrparams/const.go` (add performance test constants)

### Test Declaration

```go
var _ = Describe(
    "SBR Block Storage Performance — Ceph RBD Heartbeat Under Load",
    Ordered,
    ContinueOnFailure,
    Label(labels.OperatorSBR, labels.ComponentAgent), func() {

    It("Verify SBR heartbeats remain stable on Ceph RBD under load",
        reportxml.ID("TBD"),
        Label(
            labels.OperatorSBR,
            labels.DisruptionNonDestructive,
            labels.TierExtended,
            labels.PlatformAny,
            labels.ComponentAgent,
            labels.FrequencyWeekly,
        ), func() {
            testCephRBDHeartbeatPerformance()
        })
    })
```

### Main Test Function

```go
func testCephRBDHeartbeatPerformance() {
    var (
        rbdStorageClass   string
        sbrc              *unstructured.Unstructured
        baselineMetrics   *HeartbeatMetrics
        loadedMetrics     *HeartbeatMetrics
        recoveryMetrics   *HeartbeatMetrics
        stressPods        []*pod.Builder
    )

    // === PHASE 1: Setup ===
    By("Discovering Ceph RBD block storage class")
    rbdStorageClass = discoverCephRBDStorageClass()
    Expect(rbdStorageClass).ToNot(BeEmpty(), 
        "Ceph RBD storage class required; set via ODF or skip test")
    
    By("Creating SBRC with Ceph RBD and minimum timeout")
    sbrc = buildPerfTestSBRC(rbdStorageClass)
    err := APIClient.Create(context.TODO(), sbrc)
    Expect(err).ToNot(HaveOccurred(), "Failed to create performance test SBRC")
    
    DeferCleanup(func() {
        cleanupSBRC(sbrc)
    })
    
    By("Waiting for agent DaemonSet to reach ready state")
    waitForSBRCReady(sbrparams.SBRCBlockPerfTestName, sbrparams.ExtendedTimeout)
    
    agentPods := getAllAgentPods()
    Expect(len(agentPods)).To(BeNumerically(">=", 2), 
        "Need at least 2 worker nodes for meaningful performance test")
    
    // === PHASE 2: Baseline Monitoring ===
    By("Monitoring baseline heartbeat latency for 2 minutes")
    GinkgoWriter.Printf("Collecting baseline metrics from %d agent pods...\n", len(agentPods))
    
    baselineMetrics = monitorHeartbeatLatency(
        agentPods,
        time.Minute*2,
        time.Second*5, // Poll every 5 seconds
    )
    
    logMetricsSummary("BASELINE", baselineMetrics)
    
    // === PHASE 3: Load Injection ===
    By("Deploying stress-ng pods on all worker nodes")
    stressPods = deployStressDaemonSet()
    Expect(len(stressPods)).To(Equal(len(agentPods)), 
        "Stress pods should match worker count")
    
    DeferCleanup(func() {
        cleanupStressPods(stressPods)
    })
    
    By("Waiting for stress pods to reach Running state")
    Eventually(func() int {
        return countRunningPods(stressPods)
    }, time.Minute*2, time.Second*5).Should(Equal(len(stressPods)),
        "All stress pods should be running")
    
    By("Monitoring heartbeat latency under load for 5 minutes")
    GinkgoWriter.Printf("Load injection active - monitoring performance degradation...\n")
    
    loadedMetrics = monitorHeartbeatLatency(
        agentPods,
        time.Minute*5,
        time.Second*5,
    )
    
    logMetricsSummary("UNDER LOAD", loadedMetrics)
    
    // === PHASE 4: Recovery Monitoring ===
    By("Removing stress pods and monitoring recovery")
    cleanupStressPods(stressPods)
    stressPods = nil
    
    By("Monitoring heartbeat latency during recovery for 2 minutes")
    recoveryMetrics = monitorHeartbeatLatency(
        agentPods,
        time.Minute*2,
        time.Second*5,
    )
    
    logMetricsSummary("RECOVERY", recoveryMetrics)
    
    // === PHASE 5: Validation ===
    By("Validating heartbeat latency remains within acceptable bounds")
    
    // Critical validation: p99 latency must stay below 500ms even under load
    Expect(loadedMetrics.P99.Milliseconds()).To(BeNumerically("<", 500),
        "P99 heartbeat latency under load (%dms) exceeded 500ms threshold",
        loadedMetrics.P99.Milliseconds())
    
    // Warning-level validation: p99 degradation should be < 3x baseline
    degradationRatio := float64(loadedMetrics.P99) / float64(baselineMetrics.P99)
    if degradationRatio > 3.0 {
        GinkgoWriter.Printf("WARNING: P99 latency degraded by %.1fx (baseline=%v, loaded=%v)\n",
            degradationRatio, baselineMetrics.P99, loadedMetrics.P99)
    }
    
    By("Verifying no false-positive SBRStorageUnhealthy conditions")
    nodes := getAllWorkerNodes()
    for _, node := range nodes {
        hasUnhealthyCondition := checkSBRStorageUnhealthyCondition(node.Name)
        Expect(hasUnhealthyCondition).To(BeFalse(),
            "Node %s should not have SBRStorageUnhealthy condition during performance test",
            node.Name)
    }
    
    By("Validating sequence number continuity across all agents")
    for _, agentPod := range agentPods {
        gaps := detectSequenceGaps(agentPod, baselineMetrics.EndTime, loadedMetrics.EndTime)
        Expect(gaps).To(BeEmpty(),
            "Agent pod %s had sequence number gaps: %v", agentPod.Definition.Name, gaps)
    }
    
    By("Generating performance report")
    reportPath := generatePerformanceReport(baselineMetrics, loadedMetrics, recoveryMetrics)
    GinkgoWriter.Printf("Performance report saved to: %s\n", reportPath)
}
```

---

## Heartbeat Monitoring Strategy

### Metrics Collection Architecture

The test collects heartbeat performance data through two parallel channels:

#### 1. Prometheus Metrics Scraping

**Endpoint**: Each agent pod exposes metrics on `:8080/metrics`

**Key Metrics**:
```
# Heartbeat write duration histogram
sbr_heartbeat_write_duration_seconds_bucket{le="0.001"} 45
sbr_heartbeat_write_duration_seconds_bucket{le="0.005"} 120
sbr_heartbeat_write_duration_seconds_bucket{le="0.01"} 180
sbr_heartbeat_write_duration_seconds_bucket{le="0.05"} 220
sbr_heartbeat_write_duration_seconds_bucket{le="0.1"} 235
sbr_heartbeat_write_duration_seconds_bucket{le="0.5"} 240
sbr_heartbeat_write_duration_seconds_bucket{le="1.0"} 240
sbr_heartbeat_write_duration_seconds_bucket{le="+Inf"} 240
sbr_heartbeat_write_duration_seconds_sum 12.5
sbr_heartbeat_write_duration_seconds_count 240

# Current sequence number
sbr_heartbeat_sequence_number{node="worker-1"} 1234

# Error counters
sbr_heartbeat_write_errors_total 0
```

**Collection Implementation**:
```go
type HeartbeatMetrics struct {
    NodeName      string
    StartTime     time.Time
    EndTime       time.Time
    SampleCount   int
    P50           time.Duration
    P95           time.Duration
    P99           time.Duration
    Max           time.Duration
    Mean          time.Duration
    StdDev        time.Duration
    ErrorCount    int
    SequenceStart uint64
    SequenceEnd   uint64
    Gaps          []SequenceGap
}

type SequenceGap struct {
    TimestampStart time.Time
    TimestampEnd   time.Time
    Expected       uint64
    Actual         uint64
    GapSize        uint64
}

func collectAgentMetrics(agentPod *pod.Builder) (*AgentSnapshot, error) {
    // Get pod IP
    podIP := agentPod.Object.Status.PodIP
    if podIP == "" {
        return nil, fmt.Errorf("pod %s has no IP", agentPod.Definition.Name)
    }
    
    // Scrape metrics endpoint
    metricsURL := fmt.Sprintf("http://%s:8080/metrics", podIP)
    req, _ := http.NewRequestWithContext(context.TODO(), "GET", metricsURL, nil)
    req.Header.Set("Accept", "text/plain")
    
    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("failed to scrape metrics: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
    }
    
    // Parse Prometheus text format
    parser := expfmt.TextParser{}
    metricFamilies, err := parser.TextToMetricFamilies(resp.Body)
    if err != nil {
        return nil, fmt.Errorf("failed to parse metrics: %w", err)
    }
    
    snapshot := &AgentSnapshot{
        Timestamp: time.Now(),
        PodName:   agentPod.Definition.Name,
        NodeName:  agentPod.Object.Spec.NodeName,
    }
    
    // Extract histogram data
    if histo, ok := metricFamilies["sbr_heartbeat_write_duration_seconds"]; ok {
        snapshot.LatencyHistogram = parseHistogram(histo)
    }
    
    // Extract sequence number
    if seq, ok := metricFamilies["sbr_heartbeat_sequence_number"]; ok {
        snapshot.SequenceNumber = parseGauge(seq)
    }
    
    // Extract error count
    if errs, ok := metricFamilies["sbr_heartbeat_write_errors_total"]; ok {
        snapshot.ErrorCount = parseCounter(errs)
    }
    
    return snapshot, nil
}
```

#### 2. Agent Pod Log Parsing

**Complementary data source** for detailed timing analysis:

```go
func parseAgentLogsForLatency(agentPod *pod.Builder, since time.Time) ([]LatencySample, error) {
    logOpts := &corev1.PodLogOptions{
        SinceTime: &metav1.Time{Time: since},
        Container: "sbr-agent",
    }
    
    logs, err := agentPod.GetLog(120*time.Second, logOpts)
    if err != nil {
        return nil, err
    }
    
    // Parse log lines for heartbeat write timings
    // Example log: "Heartbeat write completed in 23.5ms, sequence=1234"
    samples := []LatencySample{}
    scanner := bufio.NewScanner(strings.NewReader(logs))
    
    for scanner.Scan() {
        line := scanner.Text()
        if !strings.Contains(line, "Heartbeat write completed") {
            continue
        }
        
        sample, err := parseLatencyLogLine(line)
        if err != nil {
            GinkgoWriter.Printf("Warning: failed to parse log line: %v\n", err)
            continue
        }
        
        samples = append(samples, sample)
    }
    
    return samples, nil
}
```

### Aggregation Strategy

```go
func monitorHeartbeatLatency(
    agentPods []*pod.Builder,
    duration time.Duration,
    pollInterval time.Duration,
) *HeartbeatMetrics {
    
    startTime := time.Now()
    endTime := startTime.Add(duration)
    
    allSamples := make(map[string][]AgentSnapshot) // keyed by pod name
    
    // Polling loop
    ticker := time.NewTicker(pollInterval)
    defer ticker.Stop()
    
    for now := range ticker.C {
        if now.After(endTime) {
            break
        }
        
        // Collect snapshot from each agent
        for _, agentPod := range agentPods {
            snapshot, err := collectAgentMetrics(agentPod)
            if err != nil {
                GinkgoWriter.Printf("Warning: failed to collect metrics from %s: %v\n",
                    agentPod.Definition.Name, err)
                continue
            }
            
            allSamples[agentPod.Definition.Name] = append(
                allSamples[agentPod.Definition.Name],
                *snapshot,
            )
        }
    }
    
    // Aggregate across all agents
    return aggregateMetrics(allSamples, startTime, endTime)
}

func aggregateMetrics(
    samples map[string][]AgentSnapshot,
    start, end time.Time,
) *HeartbeatMetrics {
    
    // Flatten all latency samples
    var allLatencies []time.Duration
    totalErrors := 0
    
    for _, podSamples := range samples {
        for _, snapshot := range podSamples {
            // Extract latency values from histogram
            latencies := extractLatencySamples(snapshot.LatencyHistogram)
            allLatencies = append(allLatencies, latencies...)
            totalErrors += snapshot.ErrorCount
        }
    }
    
    if len(allLatencies) == 0 {
        return &HeartbeatMetrics{
            StartTime: start,
            EndTime:   end,
            ErrorCount: totalErrors,
        }
    }
    
    // Sort for percentile calculation
    sort.Slice(allLatencies, func(i, j int) bool {
        return allLatencies[i] < allLatencies[j]
    })
    
    return &HeartbeatMetrics{
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
    }
}
```

---

## Load Generation Patterns

### Rationale for Load Profile

The chosen load pattern simulates realistic production conditions:

1. **CPU Load (50% across 4 cores)**
   - **Why**: Represents moderate application workload
   - **Impact**: Tests if heartbeat thread gets CPU time
   - **Real-world**: Application pods consuming CPU

2. **Memory Pressure (30% of RAM)**
   - **Why**: Forces kernel to reclaim pages
   - **Impact**: May evict buffer cache affecting block I/O
   - **Real-world**: Memory-intensive workloads causing page churn

3. **I/O Churn (2 workers, 1GB files)**
   - **Why**: Creates I/O queue contention
   - **Impact**: Competes with heartbeat writes for I/O bandwidth
   - **Real-world**: Database or log-heavy workloads

### Alternative Load Profiles (for future extensions)

**Profile A: Aggressive CPU**
```bash
stress-ng --cpu 8 --cpu-load 80 --timeout 300s
```
- Simulates compute-heavy workload
- Higher risk of heartbeat thread starvation

**Profile B: Memory Thrashing**
```bash
stress-ng --vm 4 --vm-bytes 60% --timeout 300s
```
- Simulates severe memory pressure
- Tests kernel page reclaim impact on I/O

**Profile C: I/O Storm**
```bash
stress-ng --hdd 8 --hdd-bytes 5G --io 4 --timeout 300s
```
- Simulates storage-intensive workload
- Tests Ceph cluster I/O saturation

### Load Injection Implementation

```go
func deployStressDaemonSet() []*pod.Builder {
    By("Creating stress-ng DaemonSet")
    
    ds := &appsv1.DaemonSet{
        ObjectMeta: metav1.ObjectMeta{
            Name:      sbrparams.StressDaemonSetName,
            Namespace: medik8sparams.OperatorNs,
        },
        Spec: appsv1.DaemonSetSpec{
            Selector: &metav1.LabelSelector{
                MatchLabels: map[string]string{
                    "app": "sbr-perf-stressor",
                },
            },
            Template: corev1.PodTemplateSpec{
                ObjectMeta: metav1.ObjectMeta{
                    Labels: map[string]string{
                        "app": "sbr-perf-stressor",
                    },
                },
                Spec: corev1.PodSpec{
                    NodeSelector: map[string]string{
                        "node-role.kubernetes.io/worker": "",
                    },
                    Containers: []corev1.Container{
                        {
                            Name:  "stress-ng",
                            Image: "registry.fedoraproject.org/fedora:latest",
                            Command: []string{
                                "/bin/bash",
                                "-c",
                                buildStressCommand(),
                            },
                            Resources: corev1.ResourceRequirements{
                                Limits: corev1.ResourceList{
                                    corev1.ResourceCPU:    resource.MustParse("4"),
                                    corev1.ResourceMemory: resource.MustParse("8Gi"),
                                },
                            },
                        },
                    },
                    RestartPolicy: corev1.RestartPolicyAlways,
                },
            },
        },
    }
    
    err := APIClient.Create(context.TODO(), ds)
    Expect(err).ToNot(HaveOccurred(), "Failed to create stress DaemonSet")
    
    // Wait for pods to be scheduled
    Eventually(func() int {
        pods, _ := pod.List(APIClient, medik8sparams.OperatorNs,
            metav1.ListOptions{LabelSelector: "app=sbr-perf-stressor"})
        return len(pods)
    }, time.Minute*2, time.Second*5).Should(BeNumerically(">=", 2),
        "Stress pods should be scheduled")
    
    // Return list of stress pods
    pods, _ := pod.List(APIClient, medik8sparams.OperatorNs,
        metav1.ListOptions{LabelSelector: "app=sbr-perf-stressor"})
    
    return pods
}

func buildStressCommand() string {
    return `
dnf install -y stress-ng || yum install -y stress-ng
echo "Starting stress-ng with profile: CPU=50%, MEM=30%, IO=moderate"
stress-ng \
  --cpu 4 --cpu-load 50 \
  --vm 2 --vm-bytes 30% \
  --hdd 2 --hdd-bytes 1G \
  --timeout 0 \
  --metrics-brief \
  --verbose
`
}
```

---

## Validation Criteria

### Pass/Fail Criteria

#### Hard Requirements (Test Fails If Violated)

1. **Heartbeat Latency Bounds**
   - **P99 latency < 500ms** during load injection
   - **P99 latency < 100ms** at baseline
   - **Max latency < 1000ms** at any time
   
   **Rationale**: With a 10-second timeout and 3-failure threshold, agents have ~30 seconds before self-fencing. A p99 latency of 500ms leaves ample margin (60x headroom at p99).

2. **No False-Positive Conditions**
   - **Zero** SBRStorageUnhealthy=True conditions on any node
   - **Zero** StorageBasedRemediation CRs created during test
   
   **Rationale**: The test load is not storage disruption - it's normal system activity. Any fencing trigger indicates timeout misconfiguration or excessive latency.

3. **Sequence Number Continuity**
   - **No gaps > 2** in sequence numbers during active monitoring
   - **Total gap count < 5** across entire test duration
   
   **Rationale**: Small gaps (1-2) may occur during agent restarts or pod rollouts. Larger gaps indicate missed heartbeats.

4. **Error Rate**
   - **Heartbeat write errors < 1%** of total attempts
   - **No sustained error bursts** (>5 consecutive errors)
   
   **Rationale**: Occasional errors are acceptable (e.g., transient I/O timeout). Sustained errors indicate a real problem.

#### Soft Requirements (Logged Warnings, Not Failures)

1. **Performance Degradation**
   - **P99 degradation < 3x** from baseline to loaded
   - **P50 degradation < 2x** from baseline to loaded
   
   **Example**: If baseline p99 = 20ms, loaded p99 should be < 60ms

2. **Recovery Time**
   - **Recovery to baseline within 30 seconds** of load removal
   - **P99 latency delta < 10%** between baseline and recovery phases

3. **Cross-Agent Consistency**
   - **P99 latency variance < 50%** across different agents
   - **No single agent with p99 > 2x cluster median**
   
   **Rationale**: Wide variance suggests node-specific issues (hardware, Ceph OSD placement, network).

### Validation Implementation

```go
func validatePerformanceResults(
    baseline, loaded, recovery *HeartbeatMetrics,
) {
    // === HARD REQUIREMENTS ===
    
    By("Validating hard requirement: P99 latency under load < 500ms")
    Expect(loaded.P99.Milliseconds()).To(BeNumerically("<", 500),
        "FAIL: P99 latency under load (%dms) exceeded 500ms threshold\n"+
        "This indicates heartbeat writes are too slow for the configured timeout.\n"+
        "Consider: (1) increasing sbrTimeoutSeconds, (2) tuning Ceph RBD performance, "+
        "(3) reducing MaxConsecutiveFailures",
        loaded.P99.Milliseconds())
    
    By("Validating hard requirement: P99 baseline latency < 100ms")
    Expect(baseline.P99.Milliseconds()).To(BeNumerically("<", 100),
        "FAIL: Baseline P99 latency (%dms) exceeded 100ms threshold\n"+
        "This indicates poor Ceph RBD performance even in idle state.\n"+
        "Check: Ceph cluster health, OSD performance, network latency",
        baseline.P99.Milliseconds())
    
    By("Validating hard requirement: No SBRStorageUnhealthy conditions")
    nodes := getAllWorkerNodes()
    for _, node := range nodes {
        hasCondition := checkSBRStorageUnhealthyCondition(node.Name)
        Expect(hasCondition).To(BeFalse(),
            "FAIL: Node %s has SBRStorageUnhealthy=True condition\n"+
            "This is a false-positive fencing trigger during normal load.\n"+
            "Check: node conditions, agent logs for error patterns",
            node.Name)
    }
    
    By("Validating hard requirement: Sequence number continuity")
    totalGaps := len(baseline.Gaps) + len(loaded.Gaps) + len(recovery.Gaps)
    Expect(totalGaps).To(BeNumerically("<", 5),
        "FAIL: Found %d sequence number gaps (threshold: 5)\n"+
        "Gaps indicate missed heartbeats. Review: agent logs, I/O errors, timeout tuning",
        totalGaps)
    
    largestGap := maxGapSize(baseline, loaded, recovery)
    Expect(largestGap).To(BeNumerically("<=", 2),
        "FAIL: Largest sequence gap was %d (threshold: 2)\n"+
        "Large gaps suggest sustained heartbeat failures",
        largestGap)
    
    By("Validating hard requirement: Error rate < 1%")
    totalSamples := baseline.SampleCount + loaded.SampleCount + recovery.SampleCount
    totalErrors := baseline.ErrorCount + loaded.ErrorCount + recovery.ErrorCount
    errorRate := float64(totalErrors) / float64(totalSamples)
    Expect(errorRate).To(BeNumerically("<", 0.01),
        "FAIL: Error rate %.2f%% exceeded 1%% threshold (%d errors / %d samples)\n"+
        "Check: Ceph cluster health, network stability, I/O timeouts",
        errorRate*100, totalErrors, totalSamples)
    
    // === SOFT REQUIREMENTS (Warnings) ===
    
    By("Checking soft requirement: Performance degradation < 3x")
    degradation := float64(loaded.P99) / float64(baseline.P99)
    if degradation > 3.0 {
        GinkgoWriter.Printf("WARNING: P99 latency degraded by %.1fx under load\n", degradation)
        GinkgoWriter.Printf("  Baseline P99: %v\n", baseline.P99)
        GinkgoWriter.Printf("  Loaded P99:   %v\n", loaded.P99)
        GinkgoWriter.Printf("  Recommendation: Review Ceph cluster I/O capacity\n")
    }
    
    By("Checking soft requirement: Recovery time < 30s")
    recoveryDelta := recovery.P99.Milliseconds() - baseline.P99.Milliseconds()
    recoveryDeltaPct := float64(recoveryDelta) / float64(baseline.P99.Milliseconds()) * 100
    if recoveryDeltaPct > 10.0 {
        GinkgoWriter.Printf("WARNING: P99 latency did not fully recover after load removal\n")
        GinkgoWriter.Printf("  Baseline P99: %v\n", baseline.P99)
        GinkgoWriter.Printf("  Recovery P99: %v (delta: +%.1f%%)\n", recovery.P99, recoveryDeltaPct)
        GinkgoWriter.Printf("  This may indicate sustained Ceph cluster pressure\n")
    }
}
```

---

## Data Collection and Metrics

### Performance Report Structure

The test generates a detailed JSON report saved to the artifacts directory:

```json
{
  "test_id": "TBD",
  "test_name": "Ceph RBD Heartbeat Performance Under Load",
  "cluster_version": "4.18.0",
  "odf_version": "4.18.0",
  "timestamp": "2026-10-07T15:30:00Z",
  "duration_seconds": 540,
  "storage_class": "ocs-storagecluster-ceph-rbd",
  "sbr_timeout_seconds": 10,
  "worker_node_count": 3,
  "baseline": {
    "duration_seconds": 120,
    "sample_count": 720,
    "p50_ms": 8,
    "p95_ms": 15,
    "p99_ms": 22,
    "max_ms": 45,
    "mean_ms": 9.5,
    "stddev_ms": 4.2,
    "error_count": 0,
    "sequence_gaps": []
  },
  "loaded": {
    "duration_seconds": 300,
    "sample_count": 1800,
    "p50_ms": 25,
    "p95_ms": 120,
    "p99_ms": 185,
    "max_ms": 320,
    "mean_ms": 45.3,
    "stddev_ms": 38.7,
    "error_count": 2,
    "sequence_gaps": [
      {
        "timestamp": "2026-10-07T15:35:12Z",
        "expected": 1245,
        "actual": 1247,
        "gap_size": 2
      }
    ],
    "load_profile": {
      "cpu_percent": 50,
      "memory_percent": 30,
      "io_workers": 2
    }
  },
  "recovery": {
    "duration_seconds": 120,
    "sample_count": 720,
    "p50_ms": 10,
    "p95_ms": 18,
    "p99_ms": 25,
    "max_ms": 42,
    "mean_ms": 11.2,
    "stddev_ms": 5.1,
    "error_count": 0,
    "sequence_gaps": []
  },
  "validations": {
    "p99_under_500ms": "PASS",
    "no_false_positives": "PASS",
    "sequence_continuity": "PASS",
    "error_rate_below_1pct": "PASS",
    "degradation_below_3x": "WARN (2.4x degradation)",
    "recovery_within_30s": "PASS"
  },
  "per_node_metrics": [
    {
      "node_name": "worker-0",
      "baseline_p99_ms": 20,
      "loaded_p99_ms": 175,
      "recovery_p99_ms": 23
    },
    {
      "node_name": "worker-1",
      "baseline_p99_ms": 24,
      "loaded_p99_ms": 195,
      "recovery_p99_ms": 27
    }
  ]
}
```

### Report Generation

```go
func generatePerformanceReport(
    baseline, loaded, recovery *HeartbeatMetrics,
) string {
    report := PerformanceReport{
        TestID:          "TBD",
        TestName:        "Ceph RBD Heartbeat Performance Under Load",
        ClusterVersion:  getClusterVersion(),
        ODFVersion:      getODFVersion(),
        Timestamp:       time.Now(),
        DurationSeconds: int(baseline.EndTime.Sub(baseline.StartTime).Seconds()) +
                        int(loaded.EndTime.Sub(loaded.StartTime).Seconds()) +
                        int(recovery.EndTime.Sub(recovery.StartTime).Seconds()),
        StorageClass:    discoverCephRBDStorageClass(),
        SBRTimeoutSeconds: sbrparams.SBRCTimeoutSecondsMin,
        WorkerNodeCount:   len(getAllWorkerNodes()),
        Baseline:          convertToReportMetrics(baseline),
        Loaded:            convertToReportMetrics(loaded),
        Recovery:          convertToReportMetrics(recovery),
        Validations:       runValidations(baseline, loaded, recovery),
        PerNodeMetrics:    collectPerNodeMetrics(),
    }
    
    // Serialize to JSON
    jsonData, err := json.MarshalIndent(report, "", "  ")
    Expect(err).ToNot(HaveOccurred(), "Failed to marshal performance report")
    
    // Save to artifacts directory
    artifactsDir := os.Getenv("ARTIFACT_DIR")
    if artifactsDir == "" {
        artifactsDir = "/tmp/sbr-test-artifacts"
    }
    os.MkdirAll(artifactsDir, 0755)
    
    reportPath := filepath.Join(artifactsDir, 
        fmt.Sprintf("sbr-heartbeat-performance-%s.json", 
            time.Now().Format("20060102-150405")))
    
    err = os.WriteFile(reportPath, jsonData, 0644)
    Expect(err).ToNot(HaveOccurred(), "Failed to write performance report")
    
    return reportPath
}
```

### Grafana Dashboard Integration (Future)

For continuous monitoring in CI, the performance data can be exported to Prometheus/Grafana:

```yaml
# Example Grafana dashboard panel config
- title: "SBR Heartbeat Latency - Baseline vs Load"
  targets:
    - expr: histogram_quantile(0.99, sbr_heartbeat_write_duration_seconds_bucket)
      legendFormat: "P99 Latency"
    - expr: histogram_quantile(0.95, sbr_heartbeat_write_duration_seconds_bucket)
      legendFormat: "P95 Latency"
  yaxis:
    format: "ms"
  alert:
    conditions:
      - type: query
        query: B
        reducer: last
        evaluator:
          type: gt
          params: [500]
```

---

## Integration with CI/CD

### OpenShift CI Configuration

**File**: `.ci-operator/config/medik8s/rhwa-system-tests/medik8s-rhwa-system-tests-main.yaml`

```yaml
tests:
- as: e2e-sbr-block-rbd-performance-aws-odf
  cluster_claim:
    architecture: amd64
    cloud: aws
    owner: openshift-ci
    product: ocp
    timeout: 1h30m0s
    version: "4.18"
  steps:
    test:
    - as: install-odf
      cli: latest
      commands: |
        # Install ODF operator
        oc create namespace openshift-storage
        oc apply -f - <<EOF
        apiVersion: operators.coreos.com/v1alpha1
        kind: Subscription
        metadata:
          name: odf-operator
          namespace: openshift-storage
        spec:
          channel: stable-4.18
          name: odf-operator
          source: redhat-operators
          sourceNamespace: openshift-marketplace
        EOF
        
        # Wait for ODF CSV
        oc wait --for=jsonpath='{.status.phase}'=Succeeded \
          csv/odf-operator.v4.18.0 \
          -n openshift-storage \
          --timeout=10m
        
        # Create StorageSystem
        oc apply -f - <<EOF
        apiVersion: odf.openshift.io/v1alpha1
        kind: StorageSystem
        metadata:
          name: ocs-storagecluster
          namespace: openshift-storage
        spec:
          kind: storagecluster.ocs.openshift.io/v1
          name: ocs-storagecluster
          namespace: openshift-storage
        EOF
        
        # Wait for Ceph RBD StorageClass
        oc wait --for=jsonpath='{.status.phase}'=Ready \
          storagecluster/ocs-storagecluster \
          -n openshift-storage \
          --timeout=20m
        
        # Verify StorageClass exists
        oc get storageclass ocs-storagecluster-ceph-rbd
      from: cli
      resources:
        requests:
          cpu: 100m
          memory: 200Mi
    
    - as: e2e-test
      cli: latest
      commands: |
        export SBR_RBD_STORAGE_CLASS="ocs-storagecluster-ceph-rbd"
        export GINKGO_LABEL_FILTER="labels.OperatorSBR && labels.TierExtended && labels.ComponentAgent"
        export ARTIFACT_DIR="${ARTIFACT_DIR:-/tmp/artifacts}"
        
        # Run only the Ceph RBD performance test
        ginkgo -v \
          --label-filter="${GINKGO_LABEL_FILTER}" \
          --focus="Ceph RBD.*heartbeat.*performance" \
          --timeout=30m \
          --poll-progress-after=5m \
          --junit-report="${ARTIFACT_DIR}/junit-sbr-heartbeat-perf.xml" \
          ./tests/sbr-operator/tests/
        
        # Upload performance report artifacts
        if [ -f "${ARTIFACT_DIR}"/sbr-heartbeat-performance-*.json ]; then
          echo "Performance report generated:"
          cat "${ARTIFACT_DIR}"/sbr-heartbeat-performance-*.json | jq .
        fi
      from: src
      resources:
        requests:
          cpu: 100m
          memory: 200Mi
  timeout: 2h0m0s
```

### Periodic Job Schedule

**Weekly execution** to track performance trends:

```yaml
periodics:
- name: periodic-ci-medik8s-rhwa-system-tests-main-sbr-rbd-performance-weekly
  interval: 168h  # Weekly
  cluster: build05
  spec:
    containers:
    - args:
      - --gcs-upload-secret=/secrets/gcs/service-account.json
      - --image-import-pull-secret=/etc/pull-secret/.dockerconfigjson
      - --report-credentials-file=/etc/report/credentials
      - --target=e2e-sbr-block-rbd-performance-aws-odf
      command:
      - ci-operator
      image: ci-operator:latest
      name: ""
      resources:
        requests:
          cpu: 10m
      volumeMounts:
      - mountPath: /secrets/gcs
        name: gcs-credentials
        readOnly: true
```

### Performance Trend Tracking

Store historical performance data for regression detection:

```bash
#!/bin/bash
# CI post-test script: Upload performance data to time-series database

REPORT_FILE=$(ls ${ARTIFACT_DIR}/sbr-heartbeat-performance-*.json | head -1)

if [ -f "${REPORT_FILE}" ]; then
    # Extract key metrics
    P99_BASELINE=$(jq -r '.baseline.p99_ms' "${REPORT_FILE}")
    P99_LOADED=$(jq -r '.loaded.p99_ms' "${REPORT_FILE}")
    CLUSTER_VERSION=$(jq -r '.cluster_version' "${REPORT_FILE}")
    
    # Push to InfluxDB (example)
    curl -X POST "https://influx.ci.openshift.org/write?db=sbr_performance" \
      --data-binary "
sbr_heartbeat_latency,phase=baseline,cluster=${CLUSTER_VERSION} p99=${P99_BASELINE}
sbr_heartbeat_latency,phase=loaded,cluster=${CLUSTER_VERSION} p99=${P99_LOADED}
"
fi
```

---

## Expected Outcomes

### Success Criteria Summary

| Metric | Expected Result |
|--------|-----------------|
| **P99 latency (baseline)** | < 100ms |
| **P99 latency (under load)** | < 500ms |
| **Max latency** | < 1000ms |
| **False-positive conditions** | 0 |
| **Sequence gaps** | < 5 total, max gap ≤ 2 |
| **Error rate** | < 1% |
| **Performance degradation** | P99 < 3x baseline |
| **Recovery time** | < 30s to baseline |

### Typical Results (Reference Values)

Based on upstream testing and Ceph RBD characteristics, expected values on healthy ODF cluster:

**Baseline (Idle)**:
- P50: 5-15ms
- P95: 10-25ms
- P99: 15-50ms
- Max: 30-100ms

**Under Load (CPU 50%, Mem 30%, I/O moderate)**:
- P50: 15-40ms
- P95: 60-150ms
- P99: 100-300ms
- Max: 200-600ms

**Recovery**:
- Similar to baseline within 30 seconds

### What Constitutes a "Good" vs "Bad" Result

**Excellent Performance**:
- Baseline P99 < 20ms
- Loaded P99 < 150ms
- Degradation factor < 2x
- Zero errors, zero gaps

**Acceptable Performance**:
- Baseline P99 < 50ms
- Loaded P99 < 300ms
- Degradation factor < 3x
- Errors < 0.5%, gaps < 3

**Concerning Performance** (investigate but may pass):
- Baseline P99 < 100ms
- Loaded P99 < 500ms
- Degradation factor < 4x
- Errors < 1%, gaps < 5

**Failing Performance** (test should fail):
- Baseline P99 >= 100ms
- Loaded P99 >= 500ms
- Any false-positive conditions
- Errors >= 1% or gaps >= 5

---

## Failure Scenarios and Debugging

### Common Failure Modes

#### 1. Excessive Baseline Latency

**Symptom**: Baseline P99 > 100ms

**Possible Causes**:
- Slow Ceph cluster (OSD performance issues)
- Network latency between nodes and Ceph
- Oversubscribed Ceph cluster
- Insufficient OSD resources

**Debug Steps**:
```bash
# Check Ceph cluster health
oc rsh -n openshift-storage $(oc get pods -n openshift-storage -l app=rook-ceph-tools -o name | head -1)
ceph status
ceph osd perf
ceph osd df

# Check RBD image performance
rbd bench --io-type write --io-size 4K --io-threads 1 --io-total 100M ocs-storagecluster-cephblockpool/pvc-xxx

# Check network latency to Ceph monitors
for mon in $(oc get svc -n openshift-storage -l app=rook-ceph-mon -o jsonpath='{.items[*].spec.clusterIP}'); do
  ping -c 5 $mon
done
```

**Resolution**:
- Tune Ceph OSD performance
- Add more OSDs or upgrade OSD node hardware
- Check for network bottlenecks
- Reduce Ceph cluster load

#### 2. Load-Induced Latency Spike

**Symptom**: Loaded P99 > 500ms (but baseline is fine)

**Possible Causes**:
- I/O scheduler contention on nodes
- CPU starvation of agent process
- Ceph cluster cannot handle combined load
- Memory pressure causing block I/O slowdown

**Debug Steps**:
```bash
# Check agent pod CPU throttling
oc describe pod -n openshift-storage-based-remediation <agent-pod>
# Look for: "cpu" throttling in container status

# Check node I/O wait
oc debug node/<worker-node>
chroot /host
iostat -x 1 10

# Check Ceph cluster I/O stats during test
ceph osd pool stats ocs-storagecluster-cephblockpool
```

**Resolution**:
- Increase agent pod CPU limits
- Tune I/O scheduler (e.g., use `mq-deadline` instead of `none`)
- Increase `sbrTimeoutSeconds` to accommodate higher latency
- Reduce stress load intensity

#### 3. False-Positive SBRStorageUnhealthy Condition

**Symptom**: Node gets SBRStorageUnhealthy=True during test

**Possible Causes**:
- Actual heartbeat failure (not a test failure, but real issue)
- Timeout too aggressive for the storage performance
- Sequence number gap causing peer agents to mark node unhealthy

**Debug Steps**:
```bash
# Check agent logs for heartbeat failures
oc logs -n openshift-storage-based-remediation <agent-pod> | grep -i "heartbeat\|write failed"

# Check node condition
oc describe node <worker-node> | grep -A 5 SBRStorageUnhealthy

# Check peer agent logs
oc logs -n openshift-storage-based-remediation <peer-agent-pod> | grep -i "peer\|unhealthy"
```

**Resolution**:
- Review agent logs for actual I/O errors
- Increase `sbrTimeoutSeconds` if latency is legitimately high
- Check for storage disruption during test (unrelated to test load)

#### 4. Sequence Number Gaps

**Symptom**: Multiple sequence gaps detected

**Possible Causes**:
- Agent pod restarts (normal during test)
- Sustained heartbeat write failures
- Clock skew between nodes
- Concurrent write conflicts (RBD locking issue)

**Debug Steps**:
```bash
# Check for agent pod restarts
oc get pods -n openshift-storage-based-remediation --show-labels -o wide

# Check for RBD image lock conflicts
oc rsh -n openshift-storage rook-ceph-tools
rbd lock list ocs-storagecluster-cephblockpool/pvc-xxx

# Review gap timestamps vs pod events
oc get events -n openshift-storage-based-remediation --sort-by='.lastTimestamp'
```

**Resolution**:
- Small gaps (1-2) during pod restarts are normal
- Sustained gaps indicate real heartbeat failures - review storage health
- Multiple large gaps may indicate RBD multi-writer issues

---

## Future Enhancements

### Phase 2 Features

1. **Multi-Cluster Comparison**
   - Run same test on different Ceph versions
   - Compare ODF vs upstream Rook performance
   - Benchmark different storage backends (Ceph RBD vs Portworx vs LVM)

2. **Advanced Load Profiles**
   - Network partition simulation during load
   - Ceph OSD failure injection during test
   - Combined workload: database + logging + compute

3. **Automated Regression Detection**
   - Store performance baselines per release
   - Alert on >20% degradation vs baseline
   - Track trends across ODF/OCP versions

4. **Extended Metrics**
   - Histogram bucket distribution visualization
   - Per-second latency time-series
   - Correlation with Ceph cluster metrics

### Phase 3 Features

1. **Production Simulation**
   - Realistic workload patterns (database, Kafka, etc.)
   - Gradual load ramp-up/down
   - Long-duration soak test (24h+)

2. **Capacity Planning**
   - Determine max safe cluster workload
   - Calculate optimal timeout values
   - Recommend Ceph tuning parameters

3. **Integration Testing**
   - Combined with NHC integration test
   - Test during cluster upgrade
   - Test during Ceph cluster rebalance

---

## Appendix A: Helper Function Reference

### Storage Class Discovery

```go
func discoverCephRBDStorageClass() string {
    if envClass := os.Getenv("SBR_RBD_STORAGE_CLASS"); envClass != "" {
        return envClass
    }
    
    scList, err := APIClient.StorageV1Interface.StorageClasses().List(
        context.TODO(), metav1.ListOptions{})
    Expect(err).ToNot(HaveOccurred())
    
    for idx := range scList.Items {
        if strings.Contains(scList.Items[idx].Provisioner, "rbd.csi.ceph.com") {
            GinkgoWriter.Printf("Auto-discovered Ceph RBD StorageClass: %s\n",
                scList.Items[idx].Name)
            return scList.Items[idx].Name
        }
    }
    
    Skip("No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS")
    return ""
}
```

### Statistical Functions

```go
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
```

---

## Appendix B: Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SBR_RBD_STORAGE_CLASS` | (auto-discover) | Override Ceph RBD StorageClass name |
| `SBR_PERF_TEST_DURATION_BASELINE` | 120s | Baseline monitoring duration |
| `SBR_PERF_TEST_DURATION_LOADED` | 300s | Load injection duration |
| `SBR_PERF_TEST_DURATION_RECOVERY` | 120s | Recovery monitoring duration |
| `SBR_PERF_STRESS_CPU_LOAD` | 50 | CPU load percentage per node |
| `SBR_PERF_STRESS_MEM_PCT` | 30 | Memory pressure percentage |
| `ARTIFACT_DIR` | /tmp/artifacts | Performance report output directory |

---

## Appendix C: Test Labels

```go
labels.OperatorSBR           // Test belongs to SBR operator suite
labels.ComponentAgent        // Tests SBR agent functionality
labels.TierExtended          // Long-running test (> 5 minutes)
labels.DisruptionNonDestructive // Does not cause node reboots
labels.PlatformAny           // Runs on any platform with ODF
labels.FrequencyWeekly       // Scheduled weekly in CI
```

---

## Document Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-10-07 | System Tests Team | Initial design document |

---

## References

1. [SBR Test Analysis Document](file:///home/whayutin/RHWA/CI/SBR/sbr_test_analysis.md)
2. [SBR Upstream Repository](https://github.com/medik8s/storage-based-remediation)
3. [ODF Documentation](https://access.redhat.com/documentation/en-us/red_hat_openshift_data_foundation/)
4. [Ceph RBD Performance Tuning](https://docs.ceph.com/en/latest/rbd/rbd-config-ref/)
5. [OpenShift CI Configuration](https://docs.ci.openshift.org/docs/architecture/ci-operator/)

---

**End of Document**
