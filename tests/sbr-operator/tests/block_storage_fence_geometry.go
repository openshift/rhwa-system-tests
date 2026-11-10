package tests

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD Fence Slot Geometry",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			testSBRC        *unstructured.Unstructured
			victimNodeName  string
			victimNodeID    uint16
			observerPodName string
		)

		BeforeAll(func() {
			// An NHC remediating through the SBR template would create and delete its own
			// StorageBasedRemediation CRs for the node this spec fences by hand.
			requireNoSBRNodeHealthCheck()

			By("Clearing residual Ceph OSD blocklist entries from prior tests")
			clearCephOSDBlocklist()
			testSBRC = setupFenceGeometrySBRC()
			victimNodeName, victimNodeID, observerPodName = selectHighestNodeIDVictim()
		})

		AfterAll(func() {
			if victimNodeName != "" {
				By("Clearing residual Ceph OSD blocklist entries left by the fencing")
				clearCephOSDBlocklist()

				By("Removing any StorageBasedRemediation CR left for the victim node")
				cleanupSBRCR(context.Background(), victimNodeName)

				By("Uncordoning the victim node if it is still cordoned")
				uncordonNodeIfNeeded(victimNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockFenceGeometryTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockFenceGeometryTestName)
			}
		})

		It("Fence slot geometry: the fence reaches the slot the victim reads, and fencing is only "+
			"proven after the victim's heartbeat stops",
			reportxml.ID("2031"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				victimBootID := recordFenceGeometryBaseline(victimNodeName, victimNodeID, observerPodName)
				sbrCR := createVictimRemediation(victimNodeName)
				awaitFenceInOwnSlot(victimNodeName, victimNodeID, observerPodName)
				lastHeartbeatTS := awaitVictimHeartbeatStall(victimNodeName, victimNodeID, observerPodName)
				assertFencingProvenAfterDeath(victimNodeName, lastHeartbeatTS)
				awaitVictimRecovery(victimNodeName, victimBootID)
				// NHC would delete the CR here, and nothing else removes the out-of-service taint.
				// The taint evicts the Ceph RBD CSI node plugin, so until it is gone the victim's
				// agent cannot attach the shared device and never becomes Ready.
				deleteVictimRemediation(sbrCR, victimNodeName)
				// ODF blocklists the victim's IP range in Ceph while the taint is on, and does not
				// always lift it once the taint is gone (DFBUGS-11731). Until it is lifted, the
				// victim's RBD map fails with ret=-108 and its agent never becomes Ready.
				By("Clearing the Ceph OSD blocklist left on the victim by the fencing")
				clearCephOSDBlocklist()
				awaitVictimAgentReady(victimNodeName)
			})
	})

// setupFenceGeometrySBRC creates the Block-mode SBRC this spec runs against and waits until fencing
// could actually proceed: the init Job has formatted the device, the agents are Ready, and the SBRC
// reports concurrent-writeable storage. That last gate matters — until it is confirmed the
// controller withholds fencing, and no fence is ever written into the victim's slot.
func setupFenceGeometrySBRC() *unstructured.Unstructured {
	failIfSNRRunning()

	By("Discovering the Ceph RBD block storage class")

	rbdStorageClass := discoverCephRBDStorageClass()

	By(fmt.Sprintf("Pre-cleaning any stale SBRC %q", sbrparams.SBRCBlockFenceGeometryTestName))
	deleteSBRCAndWait(
		buildSBRC(sbrparams.SBRCBlockFenceGeometryTestName, map[string]interface{}{}),
		sbrparams.SBRCBlockFenceGeometryTestName)

	By("Creating the Block-mode StorageBasedRemediationConfig on Ceph RBD")

	sbrc := buildSBRC(sbrparams.SBRCBlockFenceGeometryTestName,
		buildBlockModeSBRC(rbdStorageClass, nil))
	Expect(APIClient.Create(context.TODO(), sbrc)).To(Succeed(),
		"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCBlockFenceGeometryTestName)

	By("Waiting for the Block device-init Job to succeed")
	waitForBlockInitJobSucceeded(sbrparams.SBRCBlockFenceGeometryTestName, sbrc.GetUID())

	By("Waiting for the agent DaemonSet to be ready")
	waitForSBRCReady(sbrparams.SBRCBlockFenceGeometryTestName)

	By("Asserting the SBRC reports concurrent-writeable shared storage")

	Eventually(func() (bool, error) {
		return sbrcConcurrentWriteable(sbrparams.SBRCBlockFenceGeometryTestName)
	}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
		"SBRC %q must report status.storageValidation.concurrentWriteable=true, or fencing is "+
			"withheld and nothing is ever written to the victim's fence slot",
		sbrparams.SBRCBlockFenceGeometryTestName)

	return sbrc
}

// selectHighestNodeIDVictim picks the victim node and the agent pod the device is read through.
// The victim is the node holding the highest node ID in the on-device node map, so every slot after
// its own is still zero-filled: that is what turns a write aimed one slot too far into something
// the spec can see, and it is also the case the defect was dangerous in, because a completion check
// reading the zeroed neighbour slot reports a live node dead. Node IDs are hash-based, so the
// victim cannot be chosen; the spec skips when the node that owns the highest one hosts an SBR
// controller pod, since fencing reboots it. The observer is an agent on any other node.
func selectHighestNodeIDVictim() (string, uint16, string) {
	By("Listing the SBRC's agent pods")

	agentPods, listErr := sbrcAgentPods(sbrparams.SBRCBlockFenceGeometryTestName)
	Expect(listErr).ToNot(HaveOccurred(), "Must list the agent pods of SBRC %q",
		sbrparams.SBRCBlockFenceGeometryTestName)

	if len(agentPods) < 2 {
		Skip(fmt.Sprintf("Fencing is written by a peer, so agents on at least 2 nodes are needed; "+
			"SBRC %q has %d", sbrparams.SBRCBlockFenceGeometryTestName, len(agentPods)))
	}

	By("Reading the node map through an agent pod")

	nodeMap, jsonPayload, mapErr := readNodeMapFromPod(agentPods[0].Object.Name)
	Expect(mapErr).ToNot(HaveOccurred(), "Must read the node map through agent pod %q (payload: %s)",
		agentPods[0].Object.Name, jsonPayload)

	By("Picking the node that owns the highest node ID as the victim")

	victimNodeName, victimNodeID, pickErr := highestNodeIDEntry(nodeMap)
	Expect(pickErr).ToNot(HaveOccurred(),
		"The node map must assign a node ID to at least one node (payload: %s)", jsonPayload)
	GinkgoWriter.Printf("Victim node %q owns node ID %d, so it reads its own fence at slot index %d; "+
		"node map: %s\n", victimNodeName, victimNodeID, victimNodeID-1, jsonPayload)

	By("Skipping if the victim hosts an SBR controller pod")

	if controllerPodNodes()[victimNodeName] {
		Skip(fmt.Sprintf("Node %q owns the highest node ID but hosts an SBR controller pod, and "+
			"this spec reboots the victim", victimNodeName))
	}

	By("Selecting an observer agent pod on a node other than the victim")

	observerPodName, observerErr := agentPodNameByNode(
		sbrparams.SBRCBlockFenceGeometryTestName, victimNodeName, false)
	Expect(observerErr).ToNot(HaveOccurred(),
		"An agent pod outside node %q must be available to read the shared device", victimNodeName)
	GinkgoWriter.Printf("Observer agent pod: %q\n", observerPodName)

	return victimNodeName, victimNodeID, observerPodName
}

// recordFenceGeometryBaseline asserts the device starts from the state the spec's later assertions
// depend on — the victim's fence slot asks for nothing, and the slots after it are untouched — and
// returns the boot the victim is running, which the fencing reboot must change.
func recordFenceGeometryBaseline(
	victimNodeName string, victimNodeID uint16, observerPodName string,
) string {
	By("Recording the victim node's boot-id")

	bootID, bootErr := getNodeBootID(victimNodeName)
	Expect(bootErr).ToNot(HaveOccurred(), "Must read the boot-id of victim node %q", victimNodeName)

	By("Asserting the victim's own fence slot asks for nothing, and the fence slot after it is empty")

	hood, readErr := readFenceNeighbourhood(observerPodName, victimNodeID)
	Expect(readErr).ToNot(HaveOccurred(),
		"Must read the fence slots around node ID %d through observer pod %q",
		victimNodeID, observerPodName)
	Expect(hood.ownSlotQuiet()).To(Succeed(),
		"The fence slot of victim node %q (node ID %d) must hold no fence request before this spec "+
			"creates a remediation", victimNodeName, victimNodeID)
	Expect(hood.nextSlotEmpty()).To(Succeed(),
		"The fence slot after victim node %q's (node ID %d) must be untouched, so a fence written "+
			"there later can only have come from this remediation", victimNodeName, victimNodeID)

	if hood.nextExists {
		By("Asserting the heartbeat slot after the victim's is empty")

		Expect(heartbeatSlotUnwritten(observerPodName, hood.slotIndex+1)).To(Succeed(),
			"The heartbeat slot after victim node %q's (node ID %d) must be zero-filled: an "+
				"unparseable slot counts as 'stopped', which is how a misread slot turns into a "+
				"false proof of death", victimNodeName, victimNodeID)
	} else {
		GinkgoWriter.Printf("Victim node %q owns the last node ID (%d), so there is no slot after "+
			"its own: a write aimed one slot too far would fall outside the region and fail "+
			"outright\n", victimNodeName, victimNodeID)
	}

	return bootID
}

// createVictimRemediation creates the StorageBasedRemediation CR the peer agents act on, returns
// it, and registers the fallback teardown that deletes it again. NHC normally creates this CR; the
// spec stands in for it, so nothing deletes the CR mid-run and every condition on it can be read
// live — and the spec itself has to delete it once the victim is back, which is what removes the
// out-of-service taint.
func createVictimRemediation(victimNodeName string) *unstructured.Unstructured {
	By(fmt.Sprintf("Creating a StorageBasedRemediation CR for the victim node %q", victimNodeName))

	sbrCR := buildSBR(victimNodeName)
	Expect(APIClient.Create(context.TODO(), sbrCR)).To(Succeed(),
		"StorageBasedRemediation CR for node %q must be created", victimNodeName)

	// Fallback only: the spec deletes the CR itself after the victim is Ready. On a run that got
	// that far this is a no-op, and on a run that failed earlier it is what restores the node.
	DeferCleanup(func() {
		deleteVictimRemediation(sbrCR, victimNodeName)
	})

	return sbrCR
}

// deleteVictimRemediation deletes the victim's remediation CR and waits for the node to come out
// of remediation. The delete is a plain one, not a force-removal: the reconciler's finalizer is
// what uncordons the node and removes the out-of-service taint. AfterAll force-removes whatever is
// left behind. Safe to call twice — the second delete is a NotFound and the node is already
// restored.
func deleteVictimRemediation(sbrCR *unstructured.Unstructured, victimNodeName string) {
	By(fmt.Sprintf("Deleting the StorageBasedRemediation CR for node %q, as NHC would", victimNodeName))

	deleteErr := APIClient.Delete(context.TODO(), sbrCR)
	if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
		GinkgoWriter.Printf("Warning: delete StorageBasedRemediation/%s: %v\n",
			victimNodeName, deleteErr)
	}

	By(fmt.Sprintf("Waiting for the victim node %q to be schedulable with no %s taint",
		victimNodeName, sbrparams.OutOfServiceTaintKey))

	Eventually(func() error {
		return nodeRestoredAfterRemediation(victimNodeName)
	}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
		"Node %q must be uncordoned with no %s taint once its remediation CR is deleted",
		victimNodeName, sbrparams.OutOfServiceTaintKey)
}

// awaitFenceInOwnSlot waits for a peer agent to write the fence message into the slot the victim
// actually reads, and asserts it targets the victim.
//
// The slot after the victim's is re-checked on every poll rather than once at the end: with the
// slot offset mismatch the fence lands there instead and the victim's own slot simply never fills,
// so without this check the only symptom would be a timeout that says nothing about the cause.
// The victim's agent wipes its own fence slot when it restarts after the reboot, so the fence has
// to be observed before then; the reboot takes minutes, which leaves room.
func awaitFenceInOwnSlot(victimNodeName string, victimNodeID uint16, observerPodName string) {
	By(fmt.Sprintf("Waiting for a fence message in fence slot index %d, which node ID %d reads",
		victimNodeID-1, victimNodeID))

	var fence FenceSlot

	Eventually(func() error {
		hood, readErr := readFenceNeighbourhood(observerPodName, victimNodeID)
		if readErr != nil {
			return readErr
		}

		if neighbourErr := hood.nextSlotEmpty(); neighbourErr != nil {
			return StopTrying(fmt.Sprintf("the fence for node %q (node ID %d) was written past the "+
				"slot the victim reads, so the victim can never see it and the completion check "+
				"reads a slot the victim never writes", victimNodeName, victimNodeID)).Wrap(neighbourErr)
		}

		var fenceErr error
		fence, fenceErr = hood.fenceRequested()

		return fenceErr
	}, sbrparams.BlockFenceWriteTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"A peer agent must write the fence message into the fence slot node %q owns (node ID %d)",
		victimNodeName, victimNodeID)

	By("Asserting the fence message targets the victim's node ID")

	Expect(fence.TargetNodeID).To(Equal(victimNodeID),
		"The fence message in slot index %d must target node ID %d (node %q), not node ID %d",
		fence.SlotIndex, victimNodeID, victimNodeName, fence.TargetNodeID)
	GinkgoWriter.Printf("Fence message in slot index %d: writer node ID %d, target node ID %d, "+
		"reason %d\n", fence.SlotIndex, fence.WriterNodeID, fence.TargetNodeID, fence.Reason)
}

// awaitVictimHeartbeatStall waits for the victim's own heartbeat slot to fall behind the rest of
// the device, which is the storage-side evidence that it read the fence and went down, and returns
// the UnixNano timestamp of the last heartbeat it wrote. Every poll also checks that fencing has
// not been reported complete while the victim's slot was still fresh: that is the failure the slot
// offset mismatch produced, and it is not something a later poll recovers from.
func awaitVictimHeartbeatStall(
	victimNodeName string, victimNodeID uint16, observerPodName string,
) int64 {
	By(fmt.Sprintf("Waiting for the heartbeat slot of node ID %d to fall more than %s behind the "+
		"newest heartbeat on the device, with no %s=True while it is still fresh", victimNodeID,
		sbrparams.BlockHeartbeatStallQuietPeriod, sbrparams.FencingSucceededCondition))

	watch := newHeartbeatStallWatch(observerPodName, victimNodeName, victimNodeID,
		sbrparams.BlockHeartbeatStallQuietPeriod)

	Eventually(func() error {
		stallErr := watch.stalled()
		if errors.Is(stallErr, errFencingProvenWhileHeartbeating) {
			return StopTrying(fmt.Sprintf("fencing node %q (node ID %d) was proven against a slot "+
				"the victim does not write", victimNodeName, victimNodeID)).Wrap(stallErr)
		}

		return stallErr
	}, sbrparams.BlockVictimHeartbeatStallTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"The heartbeat slot of victim node %q (node ID %d) must go stale once the fence is in it",
		victimNodeName, victimNodeID)

	lastHeartbeatTS := watch.lastHeartbeat()
	GinkgoWriter.Printf("Victim node %q stopped heartbeating; its last heartbeat is stamped %s\n",
		victimNodeName, time.Unix(0, lastHeartbeatTS).UTC().Format(time.RFC3339Nano))

	return lastHeartbeatTS
}

// assertFencingProvenAfterDeath waits for the remediation to report success and checks the proof
// came after the silence, not before it. The transition time and the heartbeat timestamp come from
// different nodes' clocks, so only their order is asserted, never the gap.
func assertFencingProvenAfterDeath(victimNodeName string, lastHeartbeatTS int64) {
	By(fmt.Sprintf("Waiting for %s=True and %s=True on the victim's StorageBasedRemediation CR",
		sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition))

	var provenCR *unstructured.Unstructured

	Eventually(func() error {
		var provenErr error

		provenCR, provenErr = fencingProven(victimNodeName)

		return provenErr
	}, sbrparams.FencingProvenTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"StorageBasedRemediation/%s must reach %s=True and %s=True",
		victimNodeName, sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition)

	By("Asserting the FencingSucceeded transition happened after the victim's last heartbeat")

	Expect(fencingProvenAfterHeartbeat(provenCR, lastHeartbeatTS)).To(Succeed(),
		"%s for node %q must transition after the last heartbeat it wrote",
		sbrparams.FencingSucceededCondition, victimNodeName)

	By("Asserting the victim node carries the out-of-service taint")

	// Read live rather than from a recording: no NHC is involved, so the CR stays until teardown
	// deletes it and the reconciler leaves the taint in place until then.
	fencedNode, getErr := APIClient.CoreV1Interface.Nodes().Get(
		context.TODO(), victimNodeName, metav1.GetOptions{})
	Expect(getErr).ToNot(HaveOccurred(),
		"Must read node %q to check the %s taint", victimNodeName, sbrparams.OutOfServiceTaintKey)
	Expect(hasOutOfServiceTaint(fencedNode)).To(BeTrue(),
		"Fenced node %q must carry the %s taint", victimNodeName, sbrparams.OutOfServiceTaintKey)
}

// awaitVictimRecovery proves the fence reached the victim rather than merely being written: the
// node rebooted and came back. Its agent cannot be waited on yet — the out-of-service taint is
// still on the node at this point, so the CSI node plugin the agent needs is still evicted.
func awaitVictimRecovery(victimNodeName, victimBootID string) {
	By(fmt.Sprintf("Waiting for the victim node %q boot-id to change", victimNodeName))

	Eventually(func() error {
		bootID, bootErr := getNodeBootID(victimNodeName)
		if bootErr != nil {
			return bootErr
		}

		if bootID == victimBootID {
			return fmt.Errorf("node %s still has boot-id %s; waiting for the fencing reboot",
				victimNodeName, bootID)
		}

		return nil
	}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
		"Victim node %q must reboot on the fence in its own slot (boot-id must change from %q)",
		victimNodeName, victimBootID)

	newBootID, bootErr := getNodeBootID(victimNodeName)
	Expect(bootErr).ToNot(HaveOccurred(),
		"Must read the boot-id of victim node %q after the reboot", victimNodeName)
	GinkgoWriter.Printf("Victim node %q rebooted: boot-id %q -> %q\n",
		victimNodeName, victimBootID, newBootID)

	By(fmt.Sprintf("Waiting for the victim node %q to come back Ready", victimNodeName))

	Eventually(func() error {
		return nodeConditionIs(victimNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
	}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
		"Node %q must return Ready after the fencing reboot", victimNodeName)
}

// awaitVictimAgentReady waits for the rebooted victim to be serving the shared device again. It
// can only run once the remediation CR is gone: the out-of-service taint evicts the Ceph RBD CSI
// node plugin, and without it the agent cannot attach its block device and sits in
// ContainerCreating forever.
func awaitVictimAgentReady(victimNodeName string) {
	By("Waiting for the victim node's agent pod to be Ready again")

	Eventually(func() error {
		return agentPodReadyOnNode(sbrparams.SBRCBlockFenceGeometryTestName, victimNodeName)
	}, sbrparams.BlockAgentReadyAfterRebootTimeout,
		sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
		"The SBRC %q agent pod on node %q must become Ready after it rebooted",
		sbrparams.SBRCBlockFenceGeometryTestName, victimNodeName)
}
