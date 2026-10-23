package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// victimSlotState is the victim's heartbeat slot, identified while it was cut off, and the
// sequence number it stalled at. Recovery is measured from there rather than from the pre-outage
// baseline: the victim normally lands one more heartbeat between the baseline read and the rules
// taking effect, so the baseline value is already behind by the time the outage starts.
type victimSlotState struct {
	nodeID          uint16
	stalledSequence uint64
}

// partitionBaseline is the victim's state before its Ceph traffic is blocked. The test proves the
// transient partition left all of it untouched.
type partitionBaseline struct {
	bootID       string
	agentPodName string
	selfFenced   float64
	restarts     int32
}

var _ = Describe(
	"SBR Block Storage — Ceph RBD Transient Network Partition",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			testSBRC        *unstructured.Unstructured
			victimNodeName  string
			observerPodName string
		)

		BeforeAll(func() {
			requireNoSBRNodeHealthCheck()
			testSBRC = setupBlockModeSBRC(sbrparams.SBRCBlockTransientTestName)

			By("Selecting a victim worker node that hosts no SBR controller pod")

			victimNodeName = pickTargetWorkerNode()
			if victimNodeName == "" {
				Skip("No schedulable worker node found that does not host an SBR controller pod")
			}

			GinkgoWriter.Printf("Victim node for the Ceph RBD partition: %q\n", victimNodeName)

			By("Selecting an observer agent pod on a node other than the victim")

			var observerErr error

			observerPodName, observerErr = agentPodNameByNode(
				sbrparams.SBRCBlockTransientTestName, victimNodeName, false)
			Expect(observerErr).ToNot(HaveOccurred(),
				"An agent pod outside node %q must be available to read the shared device", victimNodeName)
			GinkgoWriter.Printf("Observer agent pod: %q\n", observerPodName)

			By("Verifying iptables is usable in the victim node's host namespaces")

			probePhase, probeLog := runHostCommandPod(victimNodeName, sbrparams.BlockPartitionProbePodName,
				cephRBDIPTablesProbeScript(), sbrparams.BlockPartitionPodTimeout)
			if probePhase != corev1.PodSucceeded {
				Skip(fmt.Sprintf("iptables is not usable in the host namespaces of node %q (probe pod %s): %s",
					victimNodeName, probePhase, probeLog))
			}
		})

		AfterAll(func() {
			By("Clearing any residual Ceph OSD blocklist entries from partition test")
			clearCephOSDBlocklist()

			if victimNodeName != "" {
				By("Removing any StorageBasedRemediation CR left for the victim node")
				cleanupSBRCR(context.Background(), victimNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q", sbrparams.SBRCBlockTransientTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockTransientTestName)
			}
		})

		It("Verify SBR recovers from a transient Ceph RBD network partition without fencing",
			reportxml.ID("1924"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				baseline := captureVictimBaseline(victimNodeName)
				startCephRBDPartition(victimNodeName)
				victim := awaitPartitionDetected(victimNodeName, observerPodName)
				awaitPartitionRecovery(victimNodeName)
				assertVictimNeverFenced(victimNodeName, observerPodName, baseline, victim)
			})
	})

// captureVictimBaseline records everything the test later compares against, and asserts the victim
// starts from a healthy state.
func captureVictimBaseline(victimNodeName string) partitionBaseline {
	By("Recording the victim node's boot-id")

	bootID, bootErr := getNodeBootID(victimNodeName)
	Expect(bootErr).ToNot(HaveOccurred(),
		"Must read the boot-id of node %q before the partition", victimNodeName)

	By("Recording the victim agent's sbr_self_fenced_total counter and container restart count")

	agentPodName, agentErr := agentPodNameByNode(sbrparams.SBRCBlockTransientTestName, victimNodeName, true)
	Expect(agentErr).ToNot(HaveOccurred(), "An SBR agent pod must be Running on node %q", victimNodeName)
	selfFenced, metricErr := readAgentCounter(agentPodName, sbrparams.SelfFencedMetricName)
	Expect(metricErr).ToNot(HaveOccurred(),
		"Must read %s from agent pod %q", sbrparams.SelfFencedMetricName, agentPodName)
	restarts, restartErr := agentRestartCount(agentPodName)
	Expect(restartErr).ToNot(HaveOccurred(), "Must read the restart count of agent pod %q", agentPodName)

	By("Asserting SBRStorageUnhealthy is not True on the victim node")

	Expect(nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
		corev1.ConditionFalse, "")).To(Succeed(),
		"Node %q must not report %s=True before its Ceph traffic is blocked",
		victimNodeName, sbrparams.SBRStorageUnhealthyCondition)

	return partitionBaseline{
		bootID:       bootID,
		agentPodName: agentPodName,
		selfFenced:   selfFenced,
		restarts:     restarts,
	}
}

// startCephRBDPartition starts the disruptor pod that cuts the victim off from Ceph for
// sbrparams.BlockPartitionOutage, and registers the teardown that guarantees the rules come off.
func startCephRBDPartition(victimNodeName string) {
	By(fmt.Sprintf("Starting a disruptor pod that blocks Ceph ports 3300, 6789 and 6800-7300 on the "+
		"victim node for %s", sbrparams.BlockPartitionOutage))

	disruptor := createHostCommandPod(victimNodeName, sbrparams.BlockPartitionDisruptorPodName,
		cephRBDPartitionScript(sbrparams.BlockPartitionOutage))

	DeferCleanup(func() {
		By("DeferCleanup: removing the disruptor pod and flushing leftover Ceph REJECT rules")
		deleteHostCommandPod(sbrparams.BlockPartitionDisruptorPodName)
		flushCephRBDRules(victimNodeName)
	})

	Expect(disruptor.WaitUntilRunning(medik8sparams.DefaultTimeout)).To(Succeed(),
		"Disruptor pod %q must start on node %q",
		sbrparams.BlockPartitionDisruptorPodName, victimNodeName)
}

// awaitPartitionDetected waits for a peer to report the victim storage-unhealthy, confirms nothing
// has been remediated, and returns the heartbeat node ID the victim writes to. The victim's node ID
// is not in the Kubernetes API, so it is looked up by node name in the on-device node map, read
// through the observer agent.
func awaitPartitionDetected(victimNodeName, observerPodName string) victimSlotState {
	By(fmt.Sprintf("Waiting for SBRStorageUnhealthy=True with reason %s on the victim node",
		sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout))

	Eventually(func() error {
		return nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
			corev1.ConditionTrue, sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)
	}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
		"A peer must report node %q %s=True (reason %s) while its Ceph traffic is blocked",
		victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
		sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)

	By("Asserting no StorageBasedRemediation CR exists for the victim node")

	_, pullErr := pullSBRCR(victimNodeName)
	Expect(k8serrors.IsNotFound(pullErr)).To(BeTrue(),
		"No StorageBasedRemediation CR must exist for node %q during a transient partition (get returned %v)",
		victimNodeName, pullErr)

	By("Looking up the victim's heartbeat node ID in the node map through the observer agent")

	nodeMap, jsonPayload, mapErr := readNodeMapFromPod(observerPodName)
	Expect(mapErr).ToNot(HaveOccurred(),
		"Must read the node map through observer pod %q (payload: %s)", observerPodName, jsonPayload)
	victimEntry, mapped := nodeMap.Entries[victimNodeName]
	Expect(mapped).To(BeTrue(),
		"The node map must hold an entry for victim node %q", victimNodeName)

	By("Recording the sequence the victim's heartbeat slot stalled at while it is partitioned")

	duringOutage, seqErr := heartbeatSequences(observerPodName)
	Expect(seqErr).ToNot(HaveOccurred(),
		"Must read the heartbeat region through observer pod %q", observerPodName)
	stalledSequence, present := duringOutage[victimEntry.NodeID]
	Expect(present).To(BeTrue(),
		"The victim's heartbeat slot (node ID %d) must be present while it is partitioned",
		victimEntry.NodeID)
	GinkgoWriter.Printf("Victim node %s owns heartbeat slot node ID %d, stalled at sequence %d\n",
		victimNodeName, victimEntry.NodeID, stalledSequence)

	return victimSlotState{nodeID: victimEntry.NodeID, stalledSequence: stalledSequence}
}

// awaitPartitionRecovery waits for the disruptor to serve out its outage window and remove its own
// rules, then for the victim to be reported healthy again.
func awaitPartitionRecovery(victimNodeName string) {
	By("Waiting for the disruptor pod to complete and restore Ceph traffic")

	phase, podLog := hostCommandPodOutcome(sbrparams.BlockPartitionDisruptorPodName,
		sbrparams.BlockPartitionCompletionTimeout)
	GinkgoWriter.Printf("Disruptor pod finished %s:\n%s\n", phase, podLog)
	Expect(phase).To(Equal(corev1.PodSucceeded),
		"Disruptor pod %q must remove its own Ceph REJECT rules and exit cleanly",
		sbrparams.BlockPartitionDisruptorPodName)

	By(fmt.Sprintf("Waiting for SBRStorageUnhealthy=False with reason %s on the victim node",
		sbrparams.SBRStorageUnhealthyReasonRecovered))

	Eventually(func() error {
		return nodeConditionIs(victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
			corev1.ConditionFalse, sbrparams.SBRStorageUnhealthyReasonRecovered)
	}, sbrparams.BlockPartitionRecoveryTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
		"Node %q must return to %s=False (reason %s) once its Ceph traffic is restored",
		victimNodeName, sbrparams.SBRStorageUnhealthyCondition,
		sbrparams.SBRStorageUnhealthyReasonRecovered)
}

// assertVictimNeverFenced proves the victim came back on its own: heartbeats resume, and the node
// is the same booted node it was before, with no remediation and no self-fence behind it.
func assertVictimNeverFenced(
	victimNodeName, observerPodName string, baseline partitionBaseline, victim victimSlotState,
) {
	By("Asserting the victim's heartbeat sequence number is increasing again through the observer agent")

	Eventually(func() (uint64, error) {
		sequences, seqErr := heartbeatSequences(observerPodName)
		if seqErr != nil {
			return 0, seqErr
		}

		return sequences[victim.nodeID], nil
	}, sbrparams.BlockHeartbeatResumeTimeout, sbrparams.DefaultPollInterval).Should(
		BeNumerically(">", victim.stalledSequence),
		"The victim's heartbeat slot (node ID %d) must advance past the sequence %d it stalled at "+
			"once Ceph traffic is restored", victim.nodeID, victim.stalledSequence)

	By(fmt.Sprintf("Consistently asserting for %s that the victim keeps its boot-id, stays Ready and "+
		"gets no StorageBasedRemediation CR", sbrparams.BlockNoFencingCheckDuration))

	Consistently(func() error {
		return victimUnfenced(victimNodeName, baseline.bootID)
	}, sbrparams.BlockNoFencingCheckDuration, sbrparams.BlockNoFencingCheckInterval).Should(Succeed(),
		"Node %q must not reboot, go NotReady or be remediated after a transient partition", victimNodeName)

	By("Asserting the victim agent's sbr_self_fenced_total counter and container restart count are unchanged")

	selfFenced, metricErr := readAgentCounter(baseline.agentPodName, sbrparams.SelfFencedMetricName)
	Expect(metricErr).ToNot(HaveOccurred(),
		"Must read %s from agent pod %q", sbrparams.SelfFencedMetricName, baseline.agentPodName)
	Expect(selfFenced).To(Equal(baseline.selfFenced),
		"Agent pod %q must not have self-fenced: %s went from %v to %v",
		baseline.agentPodName, sbrparams.SelfFencedMetricName, baseline.selfFenced, selfFenced)
	restarts, restartErr := agentRestartCount(baseline.agentPodName)
	Expect(restartErr).ToNot(HaveOccurred(),
		"Must read the restart count of agent pod %q", baseline.agentPodName)
	Expect(restarts).To(Equal(baseline.restarts),
		"Agent pod %q must not have restarted: restart count went from %d to %d",
		baseline.agentPodName, baseline.restarts, restarts)
}

// victimUnfenced returns nil while the node still runs the boot it started with, is Ready, and has
// no StorageBasedRemediation CR.
func victimUnfenced(nodeName, bootID string) error {
	currentBootID, bootErr := getNodeBootID(nodeName)
	if bootErr != nil {
		return bootErr
	}

	if currentBootID != bootID {
		return fmt.Errorf("node %s rebooted: boot-id %s -> %s", nodeName, bootID, currentBootID)
	}

	if readyErr := nodeConditionIs(nodeName, string(corev1.NodeReady),
		corev1.ConditionTrue, ""); readyErr != nil {
		return readyErr
	}

	_, pullErr := pullSBRCR(nodeName)
	if pullErr == nil {
		return fmt.Errorf("a StorageBasedRemediation CR exists for node %s", nodeName)
	}

	if !k8serrors.IsNotFound(pullErr) {
		return fmt.Errorf("get StorageBasedRemediation/%s: %w", nodeName, pullErr)
	}

	return nil
}
