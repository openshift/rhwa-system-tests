package tests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// cephRBDRejectReplyMatches are the same Ceph ports matched as sources, for the INPUT chain.
// Rejecting only OUTPUT still lets the replies of an already-established session reach the node, so
// a test that has to take a node's storage away completely blocks both directions.
var cephRBDRejectReplyMatches = []string{
	"-p tcp --sport 3300",
	"-p tcp --sport 6789",
	"-p tcp --match multiport --sports 6800:7300",
}

// cephRBDChainRuleScript returns one iptables command per match on the named chain for the given
// chain operation, "-I" to insert the REJECT rules or "-D" to remove them. Removals tolerate a
// missing rule so a flush can run whatever state the node is in.
func cephRBDChainRuleScript(operation, chain string, matches []string) string {
	var script strings.Builder

	for _, match := range matches {
		fmt.Fprintf(&script, "%s %s %s %s -j REJECT", nsenterIPTables, operation, chain, match)

		if operation == "-D" {
			fmt.Fprint(&script, " || true")
		}

		fmt.Fprintln(&script)
	}

	return script.String()
}

// cephRBDRuleScript returns the outbound-only Ceph REJECT rules for the given chain operation.
// Blocking OUTPUT is enough to cut a node off from Ceph; the callers that must also keep inbound
// replies out use cephRBDBidirectionalRuleScript.
func cephRBDRuleScript(operation string) string {
	return cephRBDChainRuleScript(operation, "OUTPUT", cephRBDRejectMatches)
}

// cephRBDBidirectionalRuleScript returns the Ceph REJECT rules on both the OUTPUT and the INPUT
// chain for the given chain operation, so neither a request nor a reply crosses the node's boundary.
func cephRBDBidirectionalRuleScript(operation string) string {
	return cephRBDRuleScript(operation) +
		cephRBDChainRuleScript(operation, "INPUT", cephRBDRejectReplyMatches)
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
// on a node that has no rules left to remove. It deletes both directions so one flusher cleans up
// after either injection; a rule that was never inserted is tolerated.
func cephRBDFlushScript() string {
	return cephRBDBidirectionalRuleScript("-D") + "exit 0\n"
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
//
//nolint:unparam // metricName is kept explicit so callers read at the call site which counter they sample.
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

// victimHeartbeatSlot returns the heartbeat slot the given node ID owns, read through the observer.
// The slot is located by position (index nodeID-1, the Block-mode geometry the agent and reconciler
// share) and must carry that node ID, so a heartbeat written to the wrong offset fails here instead
// of being found by its payload. Only that one 4 KB slot is read, not the whole region.
func victimHeartbeatSlot(observerPodName string, nodeID uint16) (SlotData, error) {
	if nodeID < 1 || nodeID > blockMaxNodes {
		return SlotData{}, fmt.Errorf("node ID %d is outside the Block-mode range 1-%d",
			nodeID, blockMaxNodes)
	}

	slotIdx := int(nodeID) - 1
	data, readErr := readDeviceBlocksFromPod(observerPodName, heartbeatRegionBlock+slotIdx, 1)
	if readErr != nil {
		return SlotData{}, readErr
	}

	// parseSlotData indexes the bytes it is handed, so the single slot read comes back at index 0.
	slots := parseSlotData(data)
	if len(slots) == 0 {
		return SlotData{}, fmt.Errorf("no valid heartbeat at slot index %d for node ID %d",
			slotIdx, nodeID)
	}

	slot := slots[0]
	if slot.NodeID != nodeID {
		return SlotData{}, fmt.Errorf("heartbeat slot at index %d (node ID %d's position) holds node ID %d",
			slotIdx, nodeID, slot.NodeID)
	}

	slot.SlotIndex = slotIdx

	return slot, nil
}

// newestSlotTimestamp returns the highest timestamp written into any slot. Freshness is measured
// against it rather than against the test runner's clock, which need not agree with the nodes'.
func newestSlotTimestamp(slots []SlotData) int64 {
	var newest int64

	for _, slot := range slots {
		if slot.Timestamp > newest {
			newest = slot.Timestamp
		}
	}

	return newest
}

// slotForNodeID returns the heartbeat slot carrying the given node ID.
func slotForNodeID(slots []SlotData, nodeID uint16) (SlotData, bool) {
	for _, slot := range slots {
		if slot.NodeID == nodeID {
			return slot, true
		}
	}

	return SlotData{}, false
}

// heartbeatSlotLag returns how far a node's heartbeat slot is behind the newest heartbeat on the
// device: newest timestamp minus the node's own. Both come from the device, written by the nodes
// themselves, so the measure holds regardless of what the test runner's clock says and regardless
// of when the read happened — a read delayed by half a minute still reports the same lag.
func heartbeatSlotLag(slots []SlotData, nodeID uint16) (time.Duration, error) {
	slot, found := slotForNodeID(slots, nodeID)
	if !found {
		return 0, fmt.Errorf("no heartbeat slot on the device carries node ID %d", nodeID)
	}

	return time.Duration(newestSlotTimestamp(slots) - slot.Timestamp), nil
}

// errFencingProvenWhileHeartbeating marks the controller reporting fencing complete while the
// victim's heartbeat slot was still advancing. That is exactly what the slot offset mismatch
// produced: the completion check read a slot the victim never writes, found it zero-filled, and an
// unparseable slot counts as "stopped". Callers match it to fail immediately rather than poll on.
var errFencingProvenWhileHeartbeating = errors.New(
	"fencing was proven while the victim was still heartbeating")

// heartbeatStallWatch follows one node's heartbeat slot and reports when it has fallen behind the
// rest of the device, which is the evidence from storage that the victim acted on its fence.
//
// Age is measured as the lag between the victim's slot and the newest slot on the device, never
// against the test runner's clock or against when a read returned. Both figures are written by the
// nodes themselves, so a read that is delayed — an exec into the observer pod can stall for tens of
// seconds — reports the same lag it would have reported on time, and a slow read cannot be mistaken
// for a stopped node.
type heartbeatStallWatch struct {
	observerPodName string
	nodeName        string
	nodeID          uint16
	// quietFor is how far behind the newest heartbeat the victim's slot must fall before it counts
	// as stopped.
	quietFor time.Duration
	// lastTimestamp is the UnixNano the victim itself wrote in the last heartbeat this watch saw.
	// The ordering assertion uses it because it comes from the victim's clock, like the heartbeat
	// the controller judges staleness on.
	lastTimestamp int64
}

// newHeartbeatStallWatch starts watching a node's heartbeat slot.
func newHeartbeatStallWatch(
	observerPodName, nodeName string, nodeID uint16, quietFor time.Duration,
) *heartbeatStallWatch {
	return &heartbeatStallWatch{
		observerPodName: observerPodName,
		nodeName:        nodeName,
		nodeID:          nodeID,
		quietFor:        quietFor,
	}
}

// stalled returns nil once the victim's heartbeat slot is more than quietFor behind the newest
// heartbeat on the device. Every poll also reads the victim's remediation: FencingSucceeded going
// True while the victim's slot is still fresh is returned wrapping
// errFencingProvenWhileHeartbeating, because a proof of death that arrives while the victim is
// demonstrably writing can only have come from reading the wrong slot. Every poll is logged,
// read errors included, so a stalled read shows up in the report next to the lag it produced.
// Intended as an Eventually body.
func (watch *heartbeatStallWatch) stalled() error {
	slots, readErr := readSlotDataFromPod(watch.observerPodName)
	if readErr != nil {
		GinkgoWriter.Printf("[heartbeat-stall] node %s (node ID %d): read through observer pod %s "+
			"failed: %v\n", watch.nodeName, watch.nodeID, watch.observerPodName, readErr)

		return readErr
	}

	lag, lagErr := heartbeatSlotLag(slots, watch.nodeID)
	if lagErr != nil {
		GinkgoWriter.Printf("[heartbeat-stall] node %s (node ID %d): %v\n",
			watch.nodeName, watch.nodeID, lagErr)

		return lagErr
	}

	if slot, found := slotForNodeID(slots, watch.nodeID); found {
		watch.lastTimestamp = slot.Timestamp
	}

	sbrCR, pullErr := pullSBRCR(watch.nodeName)
	if pullErr != nil {
		GinkgoWriter.Printf("[heartbeat-stall] node %s (node ID %d): lag %s, but the remediation "+
			"could not be read: %v\n", watch.nodeName, watch.nodeID, lag.Round(time.Second), pullErr)

		return fmt.Errorf("get StorageBasedRemediation/%s: %w", watch.nodeName, pullErr)
	}

	proven := conditionTrue(sbrCR, sbrparams.FencingSucceededCondition)
	GinkgoWriter.Printf("[heartbeat-stall] node %s (node ID %d): lag %s behind the newest heartbeat "+
		"(stalled at %s, %s=%t)\n", watch.nodeName, watch.nodeID, lag.Round(time.Second),
		watch.quietFor, sbrparams.FencingSucceededCondition, proven)

	// Two write cycles: a slot that fresh belongs to a node that is still writing, so no completion
	// check may have concluded otherwise.
	if proven && lag < sbrparams.BlockHeartbeatSlotMaxAge {
		return fmt.Errorf("%w: StorageBasedRemediation/%s reports %s=True while its heartbeat slot "+
			"(node ID %d) is only %s behind the newest heartbeat on the device",
			errFencingProvenWhileHeartbeating, watch.nodeName,
			sbrparams.FencingSucceededCondition, watch.nodeID, lag.Round(time.Second))
	}

	if lag <= watch.quietFor {
		return fmt.Errorf("heartbeat slot (node ID %d) of node %s is still being written: it is %s "+
			"behind the newest heartbeat on the device, and must fall more than %s behind",
			watch.nodeID, watch.nodeName, lag.Round(time.Second), watch.quietFor)
	}

	return nil
}

// lastHeartbeat is the UnixNano timestamp of the last heartbeat this watch saw the victim write.
func (watch *heartbeatStallWatch) lastHeartbeat() int64 {
	return watch.lastTimestamp
}

// nodeRestoredAfterRemediation returns nil once a node the reconciler cordoned is schedulable again
// and no longer carries the out-of-service taint, which is the state deleting its
// StorageBasedRemediation CR must leave it in. It deliberately does not require the node to be
// Ready: a node that was actually rebooted comes back on its own schedule. Intended as an
// Eventually body.
func nodeRestoredAfterRemediation(nodeName string) error {
	node, getErr := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if getErr != nil {
		return fmt.Errorf("get node %s: %w", nodeName, getErr)
	}

	if node.Spec.Unschedulable {
		return fmt.Errorf("node %s is still cordoned", nodeName)
	}

	if hasOutOfServiceTaint(node) {
		return fmt.Errorf("node %s still carries the %s taint", nodeName, sbrparams.OutOfServiceTaintKey)
	}

	return nil
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

// splitBrainState is the heartbeat geometry of the scenario as the device shows it once the victim
// is already cut off: which slot the victim owns, the UnixNano timestamp and sequence of the last
// heartbeat it managed to write, and the same slot identity and sequence for every witness.
//
// The timestamp anchors the ordering assertion: fencing may only be proven after the victim
// actually went silent, so the FencingSucceeded transition must be later than this. The sequences
// anchor the stronger one: the victim's must never move again while it is isolated and every
// witness's must keep moving, which is what makes a FencingSucceeded a proof of death rather than
// a claim. Both are read after the victim is reported unhealthy, never before the partition, since
// a slot normally lands one more heartbeat after the rules go in.
type splitBrainState struct {
	victimNodeID     uint16
	heartbeatTS      int64
	victimSequence   uint64
	witnessNodeIDs   map[string]uint16 // witness node name -> its heartbeat slot's node ID
	witnessSequences map[uint16]uint64 // witness node ID -> its heartbeat sequence at capture
}

// splitBrainBaseline is the pre-partition state the test proves the fencing left the witnesses in:
// their boot-ids and self-fence counters must be untouched when only the victim is isolated.
type splitBrainBaseline struct {
	victimBootID    string
	witnessBootIDs  map[string]string  // witness node name -> boot-id
	witnessAgents   map[string]string  // witness node name -> its SBR agent pod name
	witnessFenced   map[string]float64 // witness node name -> sbr_self_fenced_total
	witnessRestarts map[string]int32   // witness node name -> agent container restart count
}

// splitBrainNodes returns one victim and every other schedulable worker node as a witness. The
// victim must not host an SBR controller pod, so the fencing reboot does not take the operator down
// mid-test; witnesses may, since a spurious fence on any worker is exactly what the test looks for.
// It returns ("", nil) when fewer than MinWorkersForBlockSplitBrain workers are schedulable or none of
// them can be the victim. The workers are sorted so the victim is chosen deterministically across runs.
func splitBrainNodes() (string, []string, error) {
	controllerNodes := controllerPodNodes()
	nodeList, listErr := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/worker",
	})
	if listErr != nil {
		return "", nil, fmt.Errorf("list worker nodes: %w", listErr)
	}

	var schedulable []string

	for nodeIdx := range nodeList.Items {
		if isNodeSchedulable(&nodeList.Items[nodeIdx]) {
			schedulable = append(schedulable, nodeList.Items[nodeIdx].Name)
		}
	}

	if len(schedulable) < sbrparams.MinWorkersForBlockSplitBrain {
		return "", nil, nil
	}

	sort.Strings(schedulable)

	victimIdx := slices.IndexFunc(schedulable, func(name string) bool { return !controllerNodes[name] })
	if victimIdx < 0 {
		return "", nil, nil
	}

	victim := schedulable[victimIdx]

	return victim, slices.Delete(schedulable, victimIdx, victimIdx+1), nil
}

// splitBrainBaselineFor records, for every witness, its boot-id, agent pod name,
// sbr_self_fenced_total counter and agent restart count — the state fencing the isolated victim must
// leave untouched. The victim's own boot-id is read by the caller and carried through.
func splitBrainBaselineFor(victimBootID string, witnessNodes []string) (splitBrainBaseline, error) {
	baseline := splitBrainBaseline{
		victimBootID:    victimBootID,
		witnessBootIDs:  make(map[string]string, len(witnessNodes)),
		witnessAgents:   make(map[string]string, len(witnessNodes)),
		witnessFenced:   make(map[string]float64, len(witnessNodes)),
		witnessRestarts: make(map[string]int32, len(witnessNodes)),
	}

	for _, witness := range witnessNodes {
		witnessBootID, witnessBootErr := getNodeBootID(witness)
		if witnessBootErr != nil {
			return baseline, fmt.Errorf("read the boot-id of witness node %s: %w", witness, witnessBootErr)
		}

		baseline.witnessBootIDs[witness] = witnessBootID
		agentPodName, agentErr := agentPodNameByNode(sbrparams.SBRCBlockSplitBrainTestName, witness, true)
		if agentErr != nil {
			return baseline, fmt.Errorf("an SBR agent pod must be Running on witness node %s: %w",
				witness, agentErr)
		}

		baseline.witnessAgents[witness] = agentPodName
		selfFenced, metricErr := readAgentCounter(agentPodName, sbrparams.SelfFencedMetricName)
		if metricErr != nil {
			return baseline, fmt.Errorf("read %s from witness agent pod %s: %w",
				sbrparams.SelfFencedMetricName, agentPodName, metricErr)
		}

		baseline.witnessFenced[witness] = selfFenced
		restarts, restartErr := agentRestartCount(agentPodName)
		if restartErr != nil {
			return baseline, fmt.Errorf("read the restart count of witness agent pod %s: %w",
				agentPodName, restartErr)
		}

		baseline.witnessRestarts[witness] = restarts
	}

	return baseline, nil
}

// injectCephRBDRules inserts the outbound Ceph port REJECT rules through an already-running injector
// pod and returns the command output alongside the error so the caller can report it. The block
// persists (no self-terminating window), so the node stays isolated until a reboot clears the host's
// iptables rules. The rules are the same RBD REJECT set the transient partition test uses, so
// flushCephRBDRules removes exactly what was inserted.
func injectCephRBDRules(injectorPod *pod.Builder) (string, error) {
	return execCephRBDRuleScript(injectorPod, cephRBDRuleScript("-I"))
}

// injectCephRBDRulesBidirectional inserts the Ceph port REJECT rules on both the OUTPUT and the
// INPUT chain, so the node can neither reach Ceph nor receive anything back from it. Callers that
// need total storage loss use this rather than injectCephRBDRules, whose outbound-only rules leave
// inbound traffic of an established session flowing.
func injectCephRBDRulesBidirectional(injectorPod *pod.Builder) (string, error) {
	return execCephRBDRuleScript(injectorPod, cephRBDBidirectionalRuleScript("-I"))
}

// execCephRBDRuleScript runs an iptables rule script in the injector pod and returns its output
// alongside the error. "set -e" makes the first failing iptables call abort the script, so a
// partially applied rule set surfaces as an error instead of a half-isolated node.
func execCephRBDRuleScript(injectorPod *pod.Builder, script string) (string, error) {
	out, execErr := injectorPod.ExecCommand([]string{"sh", "-c", "set -e\n" + script})

	return out.String(), execErr
}

// splitBrainHeartbeatState looks every involved node's heartbeat slot up in the on-device node map,
// read through the observer pod, and reads what those slots hold right now: the victim's last
// heartbeat timestamp and sequence, and each witness's sequence. All of it comes from the device,
// not the Kubernetes API, which is the point — the victim is the one node that cannot reach the
// device, so its state is only knowable from a peer.
func splitBrainHeartbeatState(
	observerPodName, victimNodeName string, witnessNodes []string,
) (splitBrainState, error) {
	nodeMap, jsonPayload, mapErr := readNodeMapFromPod(observerPodName)
	if mapErr != nil {
		return splitBrainState{}, fmt.Errorf("read the node map through observer pod %s (payload: %s): %w",
			observerPodName, jsonPayload, mapErr)
	}

	victimEntry, mapped := nodeMap.Entries[victimNodeName]
	if !mapped {
		return splitBrainState{}, fmt.Errorf("the node map holds no entry for victim node %s", victimNodeName)
	}

	victimSlot, slotErr := victimHeartbeatSlot(observerPodName, victimEntry.NodeID)
	if slotErr != nil {
		return splitBrainState{}, fmt.Errorf(
			"read the victim's heartbeat slot (node ID %d) through observer pod %s: %w",
			victimEntry.NodeID, observerPodName, slotErr)
	}

	sequences, seqErr := heartbeatSequences(observerPodName)
	if seqErr != nil {
		return splitBrainState{}, fmt.Errorf("read the heartbeat sequences through observer pod %s: %w",
			observerPodName, seqErr)
	}

	victimSequence, written := sequences[victimEntry.NodeID]
	if !written {
		return splitBrainState{}, fmt.Errorf(
			"the victim's heartbeat slot (node ID %d) holds no readable sequence", victimEntry.NodeID)
	}

	state := splitBrainState{
		victimNodeID:     victimEntry.NodeID,
		heartbeatTS:      victimSlot.Timestamp,
		victimSequence:   victimSequence,
		witnessNodeIDs:   make(map[string]uint16, len(witnessNodes)),
		witnessSequences: make(map[uint16]uint64, len(witnessNodes)),
	}

	for _, witness := range witnessNodes {
		witnessEntry, witnessMapped := nodeMap.Entries[witness]
		if !witnessMapped {
			return splitBrainState{}, fmt.Errorf("the node map holds no entry for witness node %s", witness)
		}

		witnessSequence, witnessWritten := sequences[witnessEntry.NodeID]
		if !witnessWritten {
			return splitBrainState{}, fmt.Errorf(
				"witness node %s owns heartbeat slot node ID %d, which holds no readable sequence",
				witness, witnessEntry.NodeID)
		}

		state.witnessNodeIDs[witness] = witnessEntry.NodeID
		state.witnessSequences[witnessEntry.NodeID] = witnessSequence
	}

	return state, nil
}

// errVictimResumedHeartbeating marks the one unrecoverable outcome of a heartbeat progress check:
// the isolated victim wrote again. Callers match it to fail immediately rather than keep polling.
var errVictimResumedHeartbeating = errors.New("the isolated victim resumed heartbeating")

// splitBrainHeartbeatProgress re-reads the heartbeat region and compares it with the state captured
// once the victim was already isolated. It returns nil when every witness slot has advanced past the
// sequence it held then and the victim's still has not, which is the evidence a FencingSucceeded has
// to rest on: without it, "the controller proved death" cannot be told apart from the controller
// reading the wrong slot, and "the victim's slot did not move" cannot be told apart from a device
// read that is not working.
//
// Heartbeats tick every few seconds and unevenly, so a witness that has not advanced yet is an
// ordinary not-yet and the caller should read again after a while. A victim that has advanced is
// never a not-yet, and is returned wrapping errVictimResumedHeartbeating.
func splitBrainHeartbeatProgress(observerPodName string, state splitBrainState) error {
	sequences, seqErr := heartbeatSequences(observerPodName)
	if seqErr != nil {
		return fmt.Errorf("read the heartbeat sequences through observer pod %s: %w",
			observerPodName, seqErr)
	}

	if victimSequence, written := sequences[state.victimNodeID]; written &&
		victimSequence > state.victimSequence {
		return fmt.Errorf("%w: slot (node ID %d) went from sequence %d to %d, so fencing was proven "+
			"against a node that was still writing",
			errVictimResumedHeartbeating, state.victimNodeID, state.victimSequence, victimSequence)
	}

	witnesses := make([]string, 0, len(state.witnessNodeIDs))
	for witness := range state.witnessNodeIDs {
		witnesses = append(witnesses, witness)
	}

	sort.Strings(witnesses)

	for _, witness := range witnesses {
		nodeID := state.witnessNodeIDs[witness]
		sequence, written := sequences[nodeID]
		if !written {
			return fmt.Errorf("witness node %s owns heartbeat slot node ID %d, which holds no readable "+
				"sequence right now", witness, nodeID)
		}

		if sequence <= state.witnessSequences[nodeID] {
			return fmt.Errorf("witness node %s has not written again yet: its slot (node ID %d) is still "+
				"at sequence %d", witness, nodeID, state.witnessSequences[nodeID])
		}
	}

	return nil
}

// witnessesStillHealthy returns nil while no witness node is flagged SBRStorageUnhealthy=True and
// none has a StorageBasedRemediation CR. It is checked on every poll of every wait that runs while
// the victim is isolated, rather than over one window: the condition cycles True -> Unknown -> True
// and NHC deletes and recreates CRs along with it, so a witness wrongly flagged or remediated can
// appear and be withdrawn entirely inside a window that is not being watched. A witness flagged or
// remediated at any point means the fence landed on the wrong geometry.
//
// It deliberately checks only the condition and the CR, never Ready or schedulable: a witness may
// legitimately blip while the victim reboots, and the end state is asserted by witnessesUntouched.
func witnessesStillHealthy(witnessNodes []string) error {
	for _, witness := range witnessNodes {
		if condErr := transientNodeHasCondition(witness, false); condErr != nil {
			return condErr
		}

		_, pullErr := pullSBRCR(witness)
		if pullErr == nil {
			return fmt.Errorf("witness node %q unexpectedly received a StorageBasedRemediation CR "+
				"— split-brain isolation is broken", witness)
		}

		if !k8serrors.IsNotFound(pullErr) {
			return fmt.Errorf("get StorageBasedRemediation/%s: %w", witness, pullErr)
		}
	}

	return nil
}

// fencingProvenAfterHeartbeat checks that the victim's FencingSucceeded condition transitioned to True
// only after the last heartbeat it wrote — death is proven from the heartbeat going stale, so the
// proof cannot predate the silence. heartbeatTS is the UnixNano the victim wrote in that heartbeat.
// It reads the CR snapshot taken at the moment fencing was proven rather than re-pulling, so NHC
// deleting the CR after recovery cannot race it.
func fencingProvenAfterHeartbeat(provenCR *unstructured.Unstructured, heartbeatTS int64) error {
	lastHeartbeat := time.Unix(0, heartbeatTS)
	transition, transitionErr := fencingSucceededTransitionTime(provenCR)
	if transitionErr != nil {
		return fmt.Errorf("read the FencingSucceeded transition time from StorageBasedRemediation/%s: %w",
			provenCR.GetName(), transitionErr)
	}

	if !transition.After(lastHeartbeat) {
		return fmt.Errorf("FencingSucceeded for node %q transitioned at %s, which must be after its "+
			"last heartbeat at %s", provenCR.GetName(), transition.UTC().Format(time.RFC3339),
			lastHeartbeat.UTC().Format(time.RFC3339Nano))
	}

	return nil
}

// witnessesUntouched proves the fence hit only the victim: every witness is Ready, schedulable, still
// on its original boot-id, and its agent never self-fenced. The agent must also be the same pod with
// the same restart count, because a restarted or replaced agent starts its counter from zero and
// would read as "unchanged".
func witnessesUntouched(witnessNodes []string, baseline splitBrainBaseline) error {
	for _, witness := range witnessNodes {
		node, getErr := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), witness, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("read witness node %s for post-fence verification: %w", witness, getErr)
		}

		if !isNodeSchedulable(node) {
			return fmt.Errorf("witness node %q must remain schedulable while only the victim is fenced",
				witness)
		}

		if readyErr := nodeConditionIs(witness, string(corev1.NodeReady), corev1.ConditionTrue, ""); readyErr != nil {
			return fmt.Errorf("witness node %q must stay Ready while only the victim is fenced: %w",
				witness, readyErr)
		}

		if node.Status.NodeInfo.BootID != baseline.witnessBootIDs[witness] {
			return fmt.Errorf("witness node %q boot-id must not change (pre=%q, post=%q "+
				"— it was spuriously fenced)",
				witness, baseline.witnessBootIDs[witness], node.Status.NodeInfo.BootID)
		}

		agentPodName, agentErr := agentPodNameByNode(sbrparams.SBRCBlockSplitBrainTestName, witness, true)
		if agentErr != nil {
			return fmt.Errorf("an SBR agent pod must still be Running on witness node %s: %w",
				witness, agentErr)
		}

		if agentPodName != baseline.witnessAgents[witness] {
			return fmt.Errorf("witness node %q agent pod must not be replaced (pre=%q, post=%q)",
				witness, baseline.witnessAgents[witness], agentPodName)
		}

		restarts, restartErr := agentRestartCount(agentPodName)
		if restartErr != nil {
			return fmt.Errorf("read the restart count of witness agent pod %s: %w", agentPodName, restartErr)
		}

		if restarts != baseline.witnessRestarts[witness] {
			return fmt.Errorf("witness agent pod %q must not restart: restart count went from %d to %d",
				agentPodName, baseline.witnessRestarts[witness], restarts)
		}

		selfFenced, metricErr := readAgentCounter(baseline.witnessAgents[witness], sbrparams.SelfFencedMetricName)
		if metricErr != nil {
			return fmt.Errorf("read %s from witness agent pod %s: %w",
				sbrparams.SelfFencedMetricName, baseline.witnessAgents[witness], metricErr)
		}

		if selfFenced != baseline.witnessFenced[witness] {
			return fmt.Errorf("witness agent on node %q must not self-fence: %s went from %v to %v",
				witness, sbrparams.SelfFencedMetricName, baseline.witnessFenced[witness], selfFenced)
		}
	}

	return nil
}

// setupBlockModeNHCScenario brings up everything a Block-mode fencing test needs: a Ceph RBD
// StorageBasedRemediationConfig whose agents are Ready and whose shared storage is confirmed
// concurrent-writeable, and a NodeHealthCheck that remediates SBRStorageUnhealthy nodes through the
// SBR template. It returns the SBRC so the caller can delete it in AfterAll.
//
// The SBRC is built from the sub-helpers rather than setupBlockModeSBRC so the init Job can be
// waited on before the DaemonSet readiness wait: an init failure then surfaces as "init Job failed"
// instead of an opaque "DaemonSet not ready" timeout.
func setupBlockModeNHCScenario(sbrcName, nhcName string) *unstructured.Unstructured {
	// Remove this test's own NodeHealthCheck from a crashed prior run first: it references the SBR
	// template, so requireNoSBRNodeHealthCheck would otherwise see it and skip the test.
	By("Removing any NodeHealthCheck left by a previous run of this test")
	deleteNHCAndWait(nhcName)

	By("Verifying no other NodeHealthCheck already remediates through the SBR template")
	requireNoSBRNodeHealthCheck()
	failIfSNRRunning()

	By("Discovering the Ceph RBD block storage class")

	rbdStorageClass := discoverCephRBDStorageClass()

	By(fmt.Sprintf("Pre-cleaning any stale SBRC %q", sbrcName))
	deleteSBRCAndWait(buildSBRC(sbrcName, map[string]interface{}{}), sbrcName)

	By("Creating the Block-mode StorageBasedRemediationConfig on Ceph RBD")

	sbrc := buildSBRC(sbrcName, buildBlockModeSBRC(rbdStorageClass, nil))
	Expect(APIClient.Create(context.TODO(), sbrc)).To(Succeed(),
		"StorageBasedRemediationConfig %q must be created", sbrcName)

	DeferCleanup(func() {
		deleteSBRCAndWait(sbrc, sbrcName)
	})

	By("Waiting for the Block device-init Job to succeed")
	waitForBlockInitJobSucceeded(sbrcName, sbrc.GetUID())

	By("Waiting for the agent DaemonSet to be ready")
	waitForSBRCReady(sbrcName)

	By("Asserting the SBRC reports concurrent-writeable shared storage")

	Eventually(func() (bool, error) {
		return sbrcConcurrentWriteable(sbrcName)
	}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
		"SBRC %q must report status.storageValidation.concurrentWriteable=true before fencing "+
			"can proceed", sbrcName)

	By("Creating the NodeHealthCheck CR that remediates SBRStorageUnhealthy nodes through SBR")
	ensureSBRTemplate()

	// maxUnhealthy: 1 (rather than the shared builder's minHealthy: "51%") so exactly one node may
	// be unhealthy regardless of cluster size — "51%" can block remediation on a small cluster.
	nhc := buildNHCWithMaxUnhealthy(nhcName, 1)
	Expect(APIClient.Create(context.TODO(), nhc)).To(Succeed(),
		"NodeHealthCheck CR %q must be created", nhcName)

	return sbrc
}
