package tests

import (
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// deviceInspection accumulates everything the two readers pulled off the shared device, so the
// assertions can cross-check it and the last step can attach all of it to the test report.
type deviceInspection struct {
	readerA        string
	readerB        string
	agentNodeNames []string
	superblockRaw  []byte
	superblock     *blockSuperblock
	nodeMapA       *blockNodeMap
	nodeMapJSONA   string
	nodeMapB       *blockNodeMap
	nodeMapJSONB   string
	heartbeatsA    []SlotData
	heartbeatsB    []SlotData
	fenceSlots     map[int]FenceSlot
}

var _ = Describe(
	"SBR Block Storage — Ceph RBD Device Inspection",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentDaemonSet), func() {
		var (
			testSBRC   *unstructured.Unstructured
			inspection *deviceInspection
		)

		BeforeAll(func() {
			testSBRC = setupBlockModeSBRC(sbrparams.SBRCBlockInspectionTestName)

			By("Selecting two reader agent pods on different nodes")

			inspection = selectInspectionReaders()
		})

		AfterAll(func() {
			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockInspectionTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockInspectionTestName)
			}
		})

		It("Verify SBR device inspection works on Ceph RBD block storage",
			reportxml.ID("1926"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.ComponentDaemonSet,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				inspectSuperblock(inspection)
				inspectNodeMap(inspection)
				inspectHeartbeatSlots(inspection)
				inspectFenceSlots(inspection)
				inspectThroughSecondReader(inspection)
				saveInspectionArtifacts(inspection)
			})
	})

// selectInspectionReaders picks the two agent pods the inspection reads through. They have to be
// on different nodes: a second reader on the same node could be served by the same client-side
// state, which would make the cross-check between them prove nothing.
func selectInspectionReaders() *deviceInspection {
	agentPods, listErr := sbrcAgentPods(sbrparams.SBRCBlockInspectionTestName)
	Expect(listErr).ToNot(HaveOccurred(), "Failed to list the agent pods of SBRC %q",
		sbrparams.SBRCBlockInspectionTestName)
	Expect(agentPods).ToNot(BeEmpty(), "SBRC %q must have at least one Running agent pod",
		sbrparams.SBRCBlockInspectionTestName)
	nodeNames := make([]string, 0, len(agentPods))
	for _, agentPod := range agentPods {
		nodeNames = append(nodeNames, agentPod.Object.Spec.NodeName)
	}

	readerA := agentPods[0]
	readerBName, readerBErr := agentPodNameByNode(
		sbrparams.SBRCBlockInspectionTestName, readerA.Object.Spec.NodeName, false)
	if readerBErr != nil {
		Skip(fmt.Sprintf("Device inspection needs agent pods on two different nodes: %v", readerBErr))
	}

	GinkgoWriter.Printf("Reader A: %q on node %q\nReader B: %q\n",
		readerA.Object.Name, readerA.Object.Spec.NodeName, readerBName)

	return &deviceInspection{
		readerA:        readerA.Object.Name,
		readerB:        readerBName,
		agentNodeNames: nodeNames,
	}
}

// inspectSuperblock proves the device carries the format the agents believe they are writing.
func inspectSuperblock(inspection *deviceInspection) {
	By("Reading the superblock through reader A")

	superblock, raw, readErr := readSuperblockFromPod(inspection.readerA)
	Expect(readErr).ToNot(HaveOccurred(),
		"Must read the superblock through agent pod %q", inspection.readerA)
	inspection.superblock, inspection.superblockRaw = superblock, raw
	GinkgoWriter.Printf("Superblock: %+v\n", *superblock)

	By("Asserting the superblock magic is SBRBLK and the version is 1")

	Expect(superblock.Magic).To(Equal(superblockMagic),
		"The device must carry the SBR block format magic")
	Expect(superblock.Version).To(Equal(superblockVersion),
		"The device must carry superblock version %d", superblockVersion)

	By("Asserting the superblock CRC is valid")

	Expect(superblock.CRC).To(Equal(superblock.ComputedCRC),
		"The stored superblock CRC32 must match the checksum over its first %d bytes",
		superblockDataSize)

	By("Asserting the superblock quiesce flag is clear")

	Expect(superblock.Flags&flagQuiesce).To(BeZero(),
		"The quiesce flag must be clear outside a format migration, otherwise agents stop "+
			"heartbeating; superblock flags are %#x", superblock.Flags)
}

// inspectNodeMap proves the slot assignments on the device describe exactly the agents that are
// running, with no duplicate or out-of-range node ID.
func inspectNodeMap(inspection *deviceInspection) {
	By("Reading the node map through reader A")

	nodeMap, jsonPayload, readErr := readNodeMapFromPod(inspection.readerA)
	Expect(readErr).ToNot(HaveOccurred(),
		"Must read the node map through agent pod %q (payload: %s)", inspection.readerA, jsonPayload)
	inspection.nodeMapA, inspection.nodeMapJSONA = nodeMap, jsonPayload
	GinkgoWriter.Printf("Node map generation %d with %d entries\n",
		nodeMap.Generation, len(nodeMap.Entries))

	By("Asserting the node map has one entry per agent node")

	mappedNodes := make([]string, 0, len(nodeMap.Entries))
	for _, entry := range nodeMap.Entries {
		mappedNodes = append(mappedNodes, entry.NodeName)
	}

	slices.Sort(mappedNodes)
	expectedNodes := slices.Clone(inspection.agentNodeNames)
	slices.Sort(expectedNodes)
	Expect(mappedNodes).To(Equal(expectedNodes),
		"The node map must hold exactly one entry per node running an agent")

	By("Asserting every node ID is unique and within 1-255")

	seen := make(map[uint16]string, len(nodeMap.Entries))

	for _, entry := range nodeMap.Entries {
		Expect(entry.NodeID).To(And(BeNumerically(">=", 1), BeNumerically("<=", blockMaxNodes)),
			"Node %q has node ID %d, outside the 1-%d range the regions hold",
			entry.NodeName, entry.NodeID, blockMaxNodes)
		owner, duplicate := seen[entry.NodeID]
		Expect(duplicate).To(BeFalse(),
			"Node ID %d is assigned to both %q and %q; two agents would share one slot",
			entry.NodeID, owner, entry.NodeName)
		seen[entry.NodeID] = entry.NodeName
	}
}

// inspectHeartbeatSlots proves every mapped node owns a live slot at the index its node ID says.
func inspectHeartbeatSlots(inspection *deviceInspection) {
	By("Reading each node's heartbeat slot through reader A")

	slots, readErr := readSlotDataFromPod(inspection.readerA)
	Expect(readErr).ToNot(HaveOccurred(),
		"Must read the heartbeat region through agent pod %q", inspection.readerA)
	inspection.heartbeatsA = slots
	byNodeID := make(map[uint16]SlotData, len(slots))
	for _, slot := range slots {
		byNodeID[slot.NodeID] = slot
	}

	By("Asserting each heartbeat slot's embedded NodeID matches the node map")

	for _, entry := range inspection.nodeMapA.Entries {
		slot, found := byNodeID[entry.NodeID]
		Expect(found).To(BeTrue(),
			"Node %q is mapped to node ID %d but no heartbeat slot carries that ID",
			entry.NodeName, entry.NodeID)
		Expect(slot.SlotIndex+1).To(Equal(int(entry.NodeID)),
			"Node %q (node ID %d) wrote its heartbeat into slot index %d; Block mode puts node N "+
				"at index N-1", entry.NodeName, entry.NodeID, slot.SlotIndex)
	}

	By("Asserting each heartbeat slot's timestamp is younger than twice the heartbeat interval")

	newest := newestSlotTimestamp(slots)
	Expect(time.Since(time.Unix(0, newest))).To(BeNumerically("<", sbrparams.BlockInspectionMaxSkew),
		"The newest heartbeat on the device is %s old, so either every agent has stopped writing "+
			"or the test runner's clock is far from the cluster's",
		time.Since(time.Unix(0, newest)))

	for _, entry := range inspection.nodeMapA.Entries {
		age, ageErr := heartbeatSlotLag(slots, entry.NodeID)
		Expect(ageErr).ToNot(HaveOccurred(),
			"Must measure how far node %q (node ID %d) is behind the newest heartbeat",
			entry.NodeName, entry.NodeID)
		Expect(age).To(BeNumerically("<", sbrparams.BlockHeartbeatSlotMaxAge),
			"Node %q (node ID %d) last wrote %s before the newest heartbeat on the device; "+
				"at the default cadence a slot must be no more than %s behind",
			entry.NodeName, entry.NodeID, age, sbrparams.BlockHeartbeatSlotMaxAge)
	}
}

// inspectFenceSlots proves nothing is asking for a node to be fenced on an idle cluster.
func inspectFenceSlots(inspection *deviceInspection) {
	By("Reading each node's fence slot through reader A")

	fenceSlots, readErr := readFenceSlotsFromPod(inspection.readerA)
	Expect(readErr).ToNot(HaveOccurred(),
		"Must read the fence region through agent pod %q", inspection.readerA)
	inspection.fenceSlots = fenceSlots
	GinkgoWriter.Printf("Fence region holds %d written slot(s)\n", len(fenceSlots))

	By("Asserting no fence slot holds corrupt data")

	for slotIdx, slot := range fenceSlots {
		Expect(slot.Corrupt).To(BeFalse(),
			"Fence slot %d holds data that is not a valid SBD fence message. A healthy slot is "+
				"zero-filled or a valid message; corrupt contents must not be mistaken for empty",
			slotIdx)
	}

	By("Asserting each fence slot is empty or carries reason NONE")

	for _, entry := range inspection.nodeMapA.Entries {
		slot, written := fenceSlots[int(entry.NodeID)-1]
		if !written {
			continue
		}

		Expect(slot.Reason).To(Equal(fenceReasonNone),
			"Node %q (node ID %d) has a live fence message in slot %d: reason %#x written by "+
				"node ID %d for target %d. No node may be fenced on an idle cluster",
			entry.NodeName, entry.NodeID, slot.SlotIndex, slot.Reason,
			slot.WriterNodeID, slot.TargetNodeID)
	}
}

// inspectThroughSecondReader repeats the reads from an agent on another node. Both readers must
// see one device: the same slot assignments, and heartbeats that have only moved forward.
func inspectThroughSecondReader(inspection *deviceInspection) {
	By("Reading the node map through reader B")

	nodeMap, jsonPayload, readErr := readNodeMapFromPod(inspection.readerB)
	Expect(readErr).ToNot(HaveOccurred(),
		"Must read the node map through agent pod %q (payload: %s)", inspection.readerB, jsonPayload)
	inspection.nodeMapB, inspection.nodeMapJSONB = nodeMap, jsonPayload

	By("Asserting reader B's node map matches reader A's")

	Expect(nodeMap.Entries).To(Equal(inspection.nodeMapA.Entries),
		"Agents on different nodes must read the same slot assignments off the shared device")

	By("Reading each node's heartbeat slot through reader B")

	slots, slotErr := readSlotDataFromPod(inspection.readerB)
	Expect(slotErr).ToNot(HaveOccurred(),
		"Must read the heartbeat region through agent pod %q", inspection.readerB)
	inspection.heartbeatsB = slots

	By("Asserting each sequence number is greater than or equal to what reader A saw")

	sequencesA := sequencesByNodeID(inspection.heartbeatsA)
	sequencesB := sequencesByNodeID(slots)

	for nodeID, sequenceA := range sequencesA {
		sequenceB, found := sequencesB[nodeID]
		Expect(found).To(BeTrue(),
			"Reader %q sees no heartbeat slot for node ID %d, which reader %q read at sequence %d",
			inspection.readerB, nodeID, inspection.readerA, sequenceA)
		Expect(sequenceB).To(BeNumerically(">=", sequenceA),
			"Node ID %d went backwards between readers: %q saw sequence %d, %q then saw %d",
			nodeID, inspection.readerA, sequenceA, inspection.readerB, sequenceB)
	}
}

// sequencesByNodeID indexes parsed slots by the node ID embedded in them.
func sequencesByNodeID(slots []SlotData) map[uint16]uint64 {
	sequences := make(map[uint16]uint64, len(slots))
	for _, slot := range slots {
		sequences[slot.NodeID] = slot.Sequence
	}

	return sequences
}

// saveInspectionArtifacts attaches the decoded device state to the Ginkgo report, so a failed run
// can be diagnosed from the report alone. The raw regions are over 2 MB and almost entirely zero
// padding, so what is kept is the superblock bytes, both node map payloads, and every written
// slot - the whole of what the inspection actually read.
func saveInspectionArtifacts(inspection *deviceInspection) {
	By("Saving the raw device dumps as test artifacts")

	AddReportEntry("sbr-block-superblock-raw", fmt.Sprintf("%x", inspection.superblockRaw))
	AddReportEntry("sbr-block-superblock", fmt.Sprintf("%+v", *inspection.superblock))
	AddReportEntry("sbr-block-nodemap-reader-a", inspection.nodeMapJSONA)
	AddReportEntry("sbr-block-nodemap-reader-b", inspection.nodeMapJSONB)
	AddReportEntry("sbr-block-heartbeats-reader-a", formatSlots(inspection.heartbeatsA))
	AddReportEntry("sbr-block-heartbeats-reader-b", formatSlots(inspection.heartbeatsB))
	AddReportEntry("sbr-block-fence-slots", fmt.Sprintf("%+v", inspection.fenceSlots))
}

// formatSlots renders parsed heartbeat slots one per line, in slot order.
func formatSlots(slots []SlotData) string {
	lines := make([]string, 0, len(slots))
	for _, slot := range slots {
		lines = append(lines, fmt.Sprintf("slot=%d node_id=%d sequence=%d timestamp=%s",
			slot.SlotIndex, slot.NodeID, slot.Sequence,
			time.Unix(0, slot.Timestamp).UTC().Format(time.RFC3339Nano)))
	}

	if len(lines) == 0 {
		return "<no written slots>"
	}

	return strings.Join(lines, "\n")
}
