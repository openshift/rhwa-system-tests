package tests

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// discoverCephRBDStorageClass returns the Ceph RBD block storage class name.
// It first checks the SBR_RBD_STORAGE_CLASS environment variable, then auto-discovers
// a StorageClass with a provisioner containing "rbd.csi.ceph.com".
// If neither is available, the test is skipped.
func discoverCephRBDStorageClass() string {
	if sbrparams.SBRRBDStorageClass != "" {
		GinkgoWriter.Printf("Using Ceph RBD StorageClass from SBR_RBD_STORAGE_CLASS: %s\n",
			sbrparams.SBRRBDStorageClass)

		return sbrparams.SBRRBDStorageClass
	}

	scList, err := APIClient.StorageV1Interface.StorageClasses().List(
		context.TODO(), metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list StorageClasses for Ceph RBD auto-discovery")

	for scIdx := range scList.Items {
		provisioner := scList.Items[scIdx].Provisioner
		// ODF Ceph RBD provisioner: openshift-storage.rbd.csi.ceph.com
		// Rook Ceph RBD provisioner: rook-ceph.rbd.csi.ceph.com
		if strings.Contains(provisioner, "rbd.csi.ceph.com") {
			GinkgoWriter.Printf("Auto-discovered Ceph RBD StorageClass: %s (provisioner: %s)\n",
				scList.Items[scIdx].Name, provisioner)

			return scList.Items[scIdx].Name
		}
	}

	Skip("No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS env var")

	return ""
}

// buildBlockModeSBRC returns an unstructured StorageBasedRemediationConfig with block volume mode.
func buildBlockModeSBRC(storageClass string, additionalSpec map[string]interface{}) map[string]interface{} {
	spec := map[string]interface{}{
		"sharedStorageClass":      storageClass,
		"sharedStorageVolumeMode": "Block",
	}

	// Merge additional spec fields if provided
	for key, value := range additionalSpec {
		spec[key] = value
	}

	return spec
}

// logNodeStatus logs the current status of a node including Ready condition, bootID, and any taints.
func logNodeStatus(nodeName string) {
	node, err := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("[%s] ⚠️  Failed to get node: %v\n", nodeName, err)

		return
	}

	// Get Ready condition
	var readyStatus, readyReason string
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			readyStatus = string(cond.Status)
			readyReason = cond.Reason

			break
		}
	}

	// Get boot ID
	bootID := node.Status.NodeInfo.BootID
	if len(bootID) > 12 {
		bootID = bootID[:12] + "..." // Truncate for readability
	}

	// Check for taints
	taintInfo := "no-taints"
	if len(node.Spec.Taints) > 0 {
		taintKeys := make([]string, 0, len(node.Spec.Taints))
		for _, taint := range node.Spec.Taints {
			taintKeys = append(taintKeys, taint.Key)
		}
		taintInfo = strings.Join(taintKeys, ",")
	}

	// Format status icon
	statusIcon := "✓"
	if readyStatus != "True" {
		statusIcon = "⚠️"
	}

	GinkgoWriter.Printf("[%s] %s Ready=%s Reason=%s BootID=%s Taints=%s\n",
		nodeName, statusIcon, readyStatus, readyReason, bootID, taintInfo)
}

// nsenterIPTables runs iptables in PID 1's network and mount namespaces, so the rules land on the
// host rather than inside the pod's own namespaces.
const nsenterIPTables = "nsenter --target 1 --net --mount -- iptables"

// cephRBDRejectMatches are the iptables matches covering every port an RBD client talks to:
// 3300 (msgr2 monitor), 6789 (msgr1 monitor) and 6800-7300 (OSD/MDS).
var cephRBDRejectMatches = []string{
	"-p tcp --dport 3300",
	"-p tcp --dport 6789",
	"-p tcp --match multiport --dports 6800:7300",
}

// cephRBDRuleScript returns one iptables command per Ceph port match for the given chain
// operation, "-I" to insert the REJECT rules or "-D" to remove them. Removals tolerate a missing
// rule so a flush can run whatever state the node is in.
func cephRBDRuleScript(operation string) string {
	var script strings.Builder

	for _, match := range cephRBDRejectMatches {
		fmt.Fprintf(&script, "%s %s OUTPUT %s -j REJECT", nsenterIPTables, operation, match)

		if operation == "-D" {
			fmt.Fprint(&script, " || true")
		}

		fmt.Fprintln(&script)
	}

	return script.String()
}

// cephRBDPartitionScript blocks Ceph traffic on the host for the length of the outage and then
// removes its own rules. The removal is in an EXIT trap and the sleep is backgrounded so the rules
// also come off when the pod is deleted mid-outage: the partition must never outlive the test.
func cephRBDPartitionScript(outage time.Duration) string {
	return fmt.Sprintf(`set -e
unblock() {
%s}
trap unblock EXIT INT TERM
%ssleep %d &
wait $!
`, cephRBDRuleScript("-D"), cephRBDRuleScript("-I"), int(outage.Seconds()))
}

// cephRBDFlushScript removes the Ceph REJECT rules and always exits 0, so teardown reports success
// on a node that has no rules left to remove.
func cephRBDFlushScript() string {
	return cephRBDRuleScript("-D") + "exit 0\n"
}

// cephRBDIPTablesProbeScript lists the host OUTPUT chain. It fails on a node where iptables cannot
// be used in the host namespaces, for example an nftables-only node without the compatibility
// layer, which is what the precondition is looking for.
func cephRBDIPTablesProbeScript() string {
	return nsenterIPTables + " -L OUTPUT -n\n"
}

// createHostCommandPod creates a privileged pod on nodeName that runs script with /bin/sh in the
// host namespaces. Any pod left over from an earlier run is removed first. The caller owns the
// returned pod and must delete it with deleteHostCommandPod.
func createHostCommandPod(nodeName, podName, script string) *pod.Builder {
	deleteHostCommandPod(podName)
	builder, createErr := pod.NewBuilder(
		APIClient, podName, medik8sparams.OperatorNs, sbrparams.InjectorImage).
		DefineOnNode(nodeName).
		WithHostPid(true).
		WithPrivilegedFlag().
		WithRestartPolicy(corev1.RestartPolicyNever).
		RedefineDefaultCMD([]string{"sh", "-c", script}).
		Create()
	Expect(createErr).ToNot(HaveOccurred(),
		"Host command pod %q must be created on node %q", podName, nodeName)

	return builder
}

// deleteHostCommandPod removes a host-command pod and waits for it to be gone. A pod that is
// already absent is not an error: this runs both as pre-cleanup and as teardown.
func deleteHostCommandPod(podName string) {
	existing, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
	if pullErr != nil {
		return
	}

	if _, deleteErr := existing.DeleteAndWait(medik8sparams.DefaultTimeout); deleteErr != nil {
		GinkgoWriter.Printf("Warning: delete host command pod %s: %v\n", podName, deleteErr)
	}
}

// hostCommandPodOutcome waits for a host-command pod to terminate and returns its final phase and
// log. A pod that never terminates fails the spec; a non-zero exit is handed back to the caller,
// which decides whether that is a skip or a failure.
func hostCommandPodOutcome(podName string, timeout time.Duration) (corev1.PodPhase, string) {
	var (
		phase  corev1.PodPhase
		podLog string
	)

	Eventually(func() error {
		current, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
		if pullErr != nil {
			return fmt.Errorf("pull host command pod %s: %w", podName, pullErr)
		}

		phase = current.Object.Status.Phase
		if phase != corev1.PodSucceeded && phase != corev1.PodFailed {
			return fmt.Errorf("host command pod %s is %s, not finished", podName, phase)
		}

		podLog = hostCommandPodLog(current)

		return nil
	}, timeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"Host command pod %q must finish within %s", podName, timeout)

	return phase, podLog
}

// hostCommandPodLog returns a pod's log, or a placeholder when it cannot be read. It is only used
// to enrich messages, so an unreadable log never fails anything by itself.
func hostCommandPodLog(builder *pod.Builder) string {
	if len(builder.Definition.Spec.Containers) == 0 {
		return "<pod has no container>"
	}

	podLog, logErr := builder.GetFullLog(builder.Definition.Spec.Containers[0].Name)
	if logErr != nil {
		return fmt.Sprintf("<log unavailable: %v>", logErr)
	}

	return strings.TrimSpace(podLog)
}

// runHostCommandPod runs script to completion on nodeName and returns the pod's final phase and
// log, removing the pod before returning.
func runHostCommandPod(nodeName, podName, script string, timeout time.Duration) (corev1.PodPhase, string) {
	createHostCommandPod(nodeName, podName, script)

	defer deleteHostCommandPod(podName)

	return hostCommandPodOutcome(podName, timeout)
}

// flushCephRBDRules removes any Ceph REJECT rule left on the node. The disruptor removes its own
// rules on exit, so this is a safety net and is normally a no-op; a failure is logged rather than
// asserted so teardown never masks the test result.
func flushCephRBDRules(nodeName string) {
	phase, podLog := runHostCommandPod(nodeName, sbrparams.BlockPartitionFlusherPodName,
		cephRBDFlushScript(), sbrparams.BlockPartitionPodTimeout)
	if phase != corev1.PodSucceeded {
		GinkgoWriter.Printf("Warning: iptables flush pod on node %s ended %s: %s\n", nodeName, phase, podLog)
	}
}

// sbrcAgentPods returns the Running agent pods owned by the named SBRC's DaemonSet. Filtering by
// the DaemonSet's name prefix keeps a concurrently existing SBRC's agents out of the result.
func sbrcAgentPods(sbrcName string) ([]*pod.Builder, error) {
	agentPods, listErr := runningAgentPods()
	if listErr != nil {
		return nil, fmt.Errorf("list SBR agent pods: %w", listErr)
	}

	prefix := sbrparams.SBRAgentDaemonSetPrefix + sbrcName + "-"

	var owned []*pod.Builder

	for _, agentPod := range agentPods {
		if strings.HasPrefix(agentPod.Object.Name, prefix) {
			owned = append(owned, agentPod)
		}
	}

	return owned, nil
}

// agentPodNameOnNode returns the name of one of the SBRC's agent pods, chosen by its node:
// with onNode true, a pod running on nodeName; with onNode false, a pod running anywhere else.
func agentPodNameByNode(sbrcName, nodeName string, onNode bool) (string, error) {
	agentPods, listErr := sbrcAgentPods(sbrcName)
	if listErr != nil {
		return "", listErr
	}

	for _, agentPod := range agentPods {
		if (agentPod.Object.Spec.NodeName == nodeName) == onNode {
			return agentPod.Object.Name, nil
		}
	}

	location := "outside"
	if onNode {
		location = "on"
	}

	return "", fmt.Errorf("SBRC %s has no Running agent pod %s node %s", sbrcName, location, nodeName)
}

// agentPodReadyOnNode returns nil once the SBRC's own agent pod on nodeName reports Ready.
// agentPodNameByNode restricts the match to this SBRC's Running agents, and the Ready condition is
// what matters: it is gated on the agent's preflight-ok file, so Ready means the block write test
// passed. A missing Ready condition counts as not ready. Intended as an Eventually body.
func agentPodReadyOnNode(sbrcName, nodeName string) error {
	podName, nameErr := agentPodNameByNode(sbrcName, nodeName, true)
	if nameErr != nil {
		// No Running agent on the node yet: expected while it is still rebooting.
		return nameErr
	}

	agentPod, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
	if pullErr != nil {
		return fmt.Errorf("pull agent pod %s: %w", podName, pullErr)
	}

	for _, cond := range agentPod.Object.Status.Conditions {
		if cond.Type == corev1.PodReady {
			if cond.Status == corev1.ConditionTrue {
				return nil
			}

			return fmt.Errorf("agent pod %s is not Ready (status=%s)", podName, cond.Status)
		}
	}

	return fmt.Errorf("agent pod %s has no Ready condition yet", podName)
}

// fetchAgentMetrics returns the raw Prometheus exposition body from an SBR agent pod's own metrics
// endpoint. The API server cannot proxy to that port in OVN-K clusters, so, as in the metrics
// suite, it is fetched from inside the container. Callers that need several counters from the same
// agent scrape once and parse the body with sumPrometheusSamples.
func fetchAgentMetrics(agentPodName string) (string, error) {
	agentPod, pullErr := pod.Pull(APIClient, agentPodName, medik8sparams.OperatorNs)
	if pullErr != nil {
		return "", fmt.Errorf("pull agent pod %s: %w", agentPodName, pullErr)
	}

	metricsURL := "http://localhost:" + sbrparams.AgentMetricsPort + "/metrics"
	output, execErr := agentPod.ExecCommand([]string{
		"sh", "-c",
		"curl -sf " + metricsURL + " 2>/dev/null || wget -qO- " + metricsURL + " 2>/dev/null",
	})
	if execErr != nil {
		return "", fmt.Errorf("read metrics from agent pod %s: %w", agentPodName, execErr)
	}

	return output.String(), nil
}

// readAgentCounter returns the sum of every sample of metricName on an SBR agent pod's own metrics
// endpoint.
func readAgentCounter(agentPodName, metricName string) (float64, error) {
	body, fetchErr := fetchAgentMetrics(agentPodName)
	if fetchErr != nil {
		return 0, fetchErr
	}

	return sumPrometheusSamples(body, metricName)
}

// sumPrometheusSamples adds up every sample of metricName in a Prometheus text exposition body.
// A metric with no samples is an error rather than zero: the caller compares the value before and
// after an event, and a silently missing counter would read as "unchanged".
func sumPrometheusSamples(body, metricName string) (float64, error) {
	total := 0.0
	found := false

	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}

		// A sample line is "name value" or "name{labels} value".
		if fields[0] != metricName && !strings.HasPrefix(fields[0], metricName+"{") {
			continue
		}

		value, parseErr := strconv.ParseFloat(fields[1], 64)
		if parseErr != nil {
			return 0, fmt.Errorf("parse %s sample %q: %w", metricName, strings.TrimSpace(line), parseErr)
		}

		total += value
		found = true
	}

	if !found {
		return 0, fmt.Errorf("metric %q is absent from the agent metrics output", metricName)
	}

	return total, nil
}

// agentRestartCount returns the total container restart count of an agent pod. A node that
// self-fences reboots, which restarts its agent and resets its counters, so this is recorded
// alongside sbr_self_fenced_total to keep a reset counter from reading as "unchanged".
func agentRestartCount(agentPodName string) (int32, error) {
	agentPod, pullErr := pod.Pull(APIClient, agentPodName, medik8sparams.OperatorNs)
	if pullErr != nil {
		return 0, fmt.Errorf("pull agent pod %s: %w", agentPodName, pullErr)
	}

	var restarts int32

	for _, containerStatus := range agentPod.Object.Status.ContainerStatuses {
		restarts += containerStatus.RestartCount
	}

	return restarts, nil
}

// heartbeatSequences reads the whole heartbeat region through one agent pod and returns the
// sequence number found in each written slot, keyed by the node ID embedded in that slot.
func heartbeatSequences(observerPodName string) (map[uint16]uint64, error) {
	slots, readErr := readSlotDataFromPod(observerPodName)
	if readErr != nil {
		return nil, readErr
	}

	sequences := make(map[uint16]uint64, len(slots))
	for _, slot := range slots {
		sequences[slot.NodeID] = slot.Sequence
	}

	return sequences, nil
}

// stalledNodeIDs returns, in ascending order, the node IDs whose heartbeat sequence did not advance
// between two reads of the region. A node whose slot is missing from the later read counts as
// stalled too: either way it is not writing.
func stalledNodeIDs(before, after map[uint16]uint64) []uint16 {
	var stalled []uint16

	for nodeID, earlier := range before {
		if later, present := after[nodeID]; !present || later <= earlier {
			stalled = append(stalled, nodeID)
		}
	}

	slices.Sort(stalled)

	return stalled
}
