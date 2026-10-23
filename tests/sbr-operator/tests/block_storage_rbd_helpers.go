package tests

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
)

// getWorkerNodeCount returns the number of worker nodes in the cluster.
func getWorkerNodeCount() int {
	nodeList, err := APIClient.CoreV1Interface.Nodes().List(
		context.TODO(),
		metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker"})
	Expect(err).ToNot(HaveOccurred(), "Failed to list worker nodes")

	return len(nodeList.Items)
}

// checkSNRRunning checks if Self Node Remediation (SNR) DaemonSet pods are running.
// Returns (true, podCount) if SNR pods are running, (false, 0) otherwise.
func checkSNRRunning() (bool, int) {
	snrPodList, err := APIClient.CoreV1Interface.Pods("openshift-workload-availability").List(
		context.TODO(),
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=self-node-remediation"})
	if err != nil {
		GinkgoWriter.Printf("Warning: could not check for SNR pods: %v\n", err)

		return false, 0
	}

	runningCount := 0
	for idx := range snrPodList.Items {
		pod := &snrPodList.Items[idx]
		if pod.Status.Phase == corev1.PodRunning {
			runningCount++
		}
	}

	return runningCount > 0, runningCount
}

// On-device geometry of a Block-mode SBR device. Mirrors the operator's
// internal/blockformat/superblock.go and internal/sbdprotocol/message.go; every offset below
// is expressed in blockSlotSize units so it can be handed straight to dd's skip=/count=.
const (
	// sbrBlockDevicePath is where the agent DaemonSet attaches the shared block device
	// (agent.SharedStorageBlockDevicePath). Block mode uses volumeDevices, never volumeMounts.
	sbrBlockDevicePath = "/sbr-block"

	// blockSlotSize is BlockSlotSize: the per-slot I/O granularity in Block mode. 4 KB rather
	// than the protocol's 512-byte SBD_SLOT_SIZE so O_DIRECT works on both 512e and 4Kn devices.
	blockSlotSize = 4096

	// blockMaxNodes is BlockMaxNodes: node IDs 1-255 fill each region exactly.
	blockMaxNodes = 255

	// heartbeatRegionBlock is BlockHeartbeatRegionOffset (135168) / blockSlotSize.
	heartbeatRegionBlock = 33

	// fenceRegionBlock is BlockFenceRegionOffset (1179648) / blockSlotSize.
	fenceRegionBlock = 288

	// superblockBlock is BlockSuperblockOffset: the self-describing header at sector 0.
	superblockBlock = 0

	// nodeMapABlock is BlockNodeMapAOffset (4096) / blockSlotSize, and nodeMapBBlock is
	// BlockNodeMapBOffset (69632) / blockSlotSize. The two buffers are written alternately.
	nodeMapABlock = 1
	nodeMapBBlock = 17

	// nodeMapRegionBlocks is BlockNodeMapRegionSize (65536) / blockSlotSize.
	nodeMapRegionBlocks = 16
)

// Superblock layout, from internal/blockformat/superblock.go (little-endian throughout):
// magic[0:6], version[6:8], eight int64 region fields[8:72], flags[72:76], CRC32[76:80].
const (
	// superblockMagic is SuperblockMagic, the first 6 bytes of a formatted SBR block device.
	superblockMagic = "SBRBLK"

	// superblockVersion is SuperblockVersion, the only format version that exists.
	superblockVersion uint16 = 1

	// superblockDataSize is SuperblockDataSize: the bytes the CRC32 covers.
	superblockDataSize = 76

	// superblockTotalSize is SuperblockTotalSize: the wire size of the header, CRC included.
	superblockTotalSize = 80

	// superblockRegionFields is the number of int64 offset/length pairs between the version and
	// the flags: node map A and B, the heartbeat region and the fence region.
	superblockRegionFields = 8

	// flagQuiesce is FlagQuiesce, superblock flag bit 0: agents stop heartbeating while it is set,
	// which only happens during an operator-orchestrated format migration.
	flagQuiesce uint32 = 1 << 0
)

// Node map buffer layout, from internal/blockformat/blocknodemap.go: each 64 KB buffer is
// [Generation u64][WriterUUID 16B][PayloadLen u32][payload][pad][CRC32 u32], with the CRC32 in the
// last four bytes covering everything before it.
const (
	// nodeMapPayloadLenOffset is nmPayloadLenOffset.
	nodeMapPayloadLenOffset = 24

	// nodeMapPayloadOffset is nmPayloadOffset: where the CRC32-prefixed JSON starts.
	nodeMapPayloadOffset = 28

	// nodeMapRegionSize is BlockNodeMapRegionSize.
	nodeMapRegionSize = nodeMapRegionBlocks * blockSlotSize

	// nodeMapCRCOffset is where parseBuffer reads the envelope CRC32: RegionSize - 4.
	nodeMapCRCOffset = nodeMapRegionSize - 4

	// nodeMapJSONCRCSize is the CRC32 that NodeMapTable.Marshal prepends to its JSON.
	nodeMapJSONCRCSize = 4

	// nodeMapMagic is SBD_NODE_MAP_MAGIC, carried inside the JSON rather than on the envelope.
	nodeMapMagic = "SBDNMAP1"
)

// SBD message layout, from internal/sbdprotocol/message.go (little-endian throughout).
const (
	// sbdMagic is SBD_MAGIC, the first 8 bytes of every SBD message.
	sbdMagic = "SBDMSG01"

	// sbdHeaderSize is SBD_HEADER_SIZE: 8 + 2 + 1 + 2 + 8 + 8 + 4.
	sbdHeaderSize = 33

	// sbdHeartbeatMsgType is SBD_MSG_TYPE_HEARTBEAT.
	sbdHeartbeatMsgType byte = 0x01

	// sbdFenceMsgType is SBD_MSG_TYPE_FENCE.
	sbdFenceMsgType byte = 0x02

	// sbdFenceMsgSize is a marshalled fence message: the header plus TargetNodeID and Reason.
	sbdFenceMsgSize = sbdHeaderSize + 3

	// fenceReasonNone is FENCE_REASON_NONE: a fence slot that asks for nothing.
	fenceReasonNone uint8 = 0x00
)

// SlotData represents a single heartbeat slot entry from the SBR device.
type SlotData struct {
	// SlotIndex is the slot's position in the region. In Block mode the slot for node N is at
	// index N-1, so a slot whose embedded NodeID is not SlotIndex+1 was written to the wrong place.
	SlotIndex int
	NodeID    uint16
	Timestamp int64
	Sequence  uint64
	HasData   bool
}

// FenceSlot is a single parsed fence slot from the SBR device.
type FenceSlot struct {
	// SlotIndex is the slot's position in the fence region; the slot for node N is at index N-1.
	SlotIndex int
	// WriterNodeID is the node that wrote the fence message, from the SBD header.
	WriterNodeID uint16
	TargetNodeID uint16
	Reason       uint8
	// Corrupt is true when the slot holds data that is not a valid SBD fence message. Such a slot
	// must not be mistaken for an empty (never-written) slot.
	Corrupt bool
}

// readDeviceBlocksFromPod reads countBlocks blockSlotSize-sized blocks from the shared block
// device, starting at block skipBlocks, by running dd inside an SBR agent pod. iflag=direct
// bypasses the page cache so writes made by agents on other nodes are actually observed.
func readDeviceBlocksFromPod(podName string, skipBlocks, countBlocks int) ([]byte, error) {
	cmd := []string{
		"dd", "if=" + sbrBlockDevicePath,
		"bs=" + strconv.Itoa(blockSlotSize),
		"skip=" + strconv.Itoa(skipBlocks),
		"count=" + strconv.Itoa(countBlocks),
		"iflag=direct", "status=none",
	}

	return execDeviceRead(podName, cmd, countBlocks*blockSlotSize)
}

// readDeviceRegionFromPod reads regionSize bytes starting at byteOffset in a single dd read,
// rather than one read per block. A region that is checksummed as a whole, like a node map
// buffer, has to be read that way: the agents rewrite those buffers continuously, and a read
// split over several syscalls can return the first blocks from one generation and the rest from
// the next, which fails the checksum even though nothing is wrong with the device.
//
// skip_bytes lets the offset be expressed in bytes while bs covers the whole region. Both node
// map offsets are 4 KB aligned, which is what O_DIRECT needs.
func readDeviceRegionFromPod(podName string, byteOffset, regionSize int) ([]byte, error) {
	cmd := []string{
		"dd", "if=" + sbrBlockDevicePath,
		"bs=" + strconv.Itoa(regionSize),
		"skip=" + strconv.Itoa(byteOffset),
		"count=1",
		"iflag=direct,skip_bytes", "status=none",
	}

	return execDeviceRead(podName, cmd, regionSize)
}

// execDeviceRead runs a dd command inside an SBR agent pod and returns its stdout, insisting on
// exactly wantBytes of it.
func execDeviceRead(podName string, cmd []string, wantBytes int) ([]byte, error) {
	podBuilder, err := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
	if err != nil {
		return nil, fmt.Errorf("failed to pull pod %s: %w", podName, err)
	}

	var stdout, stderr bytes.Buffer

	// TTY must stay off: a pseudo-terminal translates every 0x0A byte into 0x0D 0x0A
	// on output, which shifts the raw device bytes and corrupts the slot layout.
	// ExecCommand and ExecCommandWithTimeout both force TTY on, so use the
	// context-based variant that lets the exec options through unchanged.
	err = podBuilder.ExecCommandWithContext(
		context.TODO(),
		corev1.PodExecOptions{
			Container: podBuilder.Definition.Spec.Containers[0].Name,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		},
		remotecommand.StreamOptions{
			Stdout: &stdout,
			Stderr: &stderr,
			Tty:    false,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to exec %v in pod %s: %w (stderr: %s)",
			cmd, podName, err, strings.TrimSpace(stderr.String()))
	}

	// A short read means the device is smaller than the layout requires, or dd was cut off.
	// Either way the bytes cannot be parsed against fixed offsets, so fail rather than
	// silently returning a truncated region.
	if stdout.Len() != wantBytes {
		return nil, fmt.Errorf("short read of %s in pod %s: got %d bytes, want %d (stderr: %s)",
			sbrBlockDevicePath, podName, stdout.Len(), wantBytes, strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// readSlotDataFromPod reads the whole heartbeat region from an SBR agent pod and parses
// every written heartbeat slot out of it.
func readSlotDataFromPod(podName string) ([]SlotData, error) {
	data, err := readDeviceBlocksFromPod(podName, heartbeatRegionBlock, blockMaxNodes)
	if err != nil {
		return nil, err
	}

	return parseSlotData(data), nil
}

// validSBDHeader returns the slot's message bytes when the slot at slotIdx carries a valid SBD
// header of msgType, and nil otherwise.
func validSBDHeader(data []byte, slotIdx int, msgType byte, msgSize int) []byte {
	slotBytes := data[slotIdx*blockSlotSize : slotIdx*blockSlotSize+msgSize]

	if !bytes.Equal(slotBytes[0:8], []byte(sbdMagic)) || slotBytes[10] != msgType {
		return nil
	}

	want := crc32.ChecksumIEEE(slotBytes[0:29])
	if got := binary.LittleEndian.Uint32(slotBytes[29:33]); got != want {
		GinkgoWriter.Printf("  Slot %d: checksum mismatch (got %d, want %d), skipping\n",
			slotIdx, got, want)

		return nil
	}

	return slotBytes
}

// parseSlotData parses raw heartbeat-region bytes into SlotData entries.
//
// Each slot is BlockSlotSize (4096) bytes and starts with a packed 33-byte SBD
// message header, little-endian throughout
// (internal/sbdprotocol/message.go Marshal):
//
//	[0:8]   Magic "SBDMSG01"
//	[8:10]  Version   uint16
//	[10]    Type      byte (0x01 heartbeat, 0x02 fence)
//	[11:13] NodeID    uint16
//	[13:21] Timestamp uint64 (UnixNano)
//	[21:29] Sequence  uint64
//	[29:33] Checksum  uint32 (CRC32 over bytes 0:29)
//
// The slot for node N is at index N-1 in Block mode, so the returned SlotData
// carries the NodeID read from the payload rather than the slot index.
func parseSlotData(data []byte) []SlotData {
	var slots []SlotData

	for slotIdx := 0; slotIdx < len(data)/blockSlotSize && slotIdx < blockMaxNodes; slotIdx++ {
		slotBytes := validSBDHeader(data, slotIdx, sbdHeartbeatMsgType, sbdHeaderSize)

		if slotBytes == nil {
			continue
		}

		slots = append(slots, SlotData{
			SlotIndex: slotIdx,
			NodeID:    binary.LittleEndian.Uint16(slotBytes[11:13]),
			Timestamp: int64(binary.LittleEndian.Uint64(slotBytes[13:21])),
			Sequence:  binary.LittleEndian.Uint64(slotBytes[21:29]),
			HasData:   true,
		})
	}

	return slots
}

// validateSlotDataConsistency compares slot data across all pods to ensure
// they all see the same block device content (data integrity).
func validateSlotDataConsistency(slotDataByPod map[string][]SlotData) error {
	if len(slotDataByPod) == 0 {
		return errors.New("no slot data collected from any pods")
	}

	// Get reference slot count from first pod
	var referencePodName string
	var referenceSlotCount int

	for podName, slots := range slotDataByPod {
		referencePodName = podName
		referenceSlotCount = len(slots)

		break
	}

	GinkgoWriter.Printf("Using pod %s as reference with %d slots\n", referencePodName, referenceSlotCount)

	// Compare slot counts across all pods
	totalSlots := 0
	minSlots := referenceSlotCount
	maxSlots := referenceSlotCount

	for podName, slots := range slotDataByPod {
		slotCount := len(slots)
		totalSlots += slotCount

		if slotCount < minSlots {
			minSlots = slotCount
		}
		if slotCount > maxSlots {
			maxSlots = slotCount
		}

		GinkgoWriter.Printf("  Pod %s: %d slots with data\n", podName, slotCount)
	}

	// Allow some variance due to timing (agents writing at different times)
	// but slots should be relatively consistent
	variance := maxSlots - minSlots
	maxAllowedVariance := 5 // Allow up to 5 slot difference

	if variance > maxAllowedVariance {
		return fmt.Errorf("slot count variance too high (%d), min=%d max=%d: "+
			"block device reads are inconsistent across pods",
			variance, minSlots, maxSlots)
	}
	GinkgoWriter.Printf("✓ Slot counts are consistent across pods (variance: %d, max allowed: %d)\n",
		variance, maxAllowedVariance)

	return nil
}

// validateSequenceNumbers proves that every expected node is still writing heartbeats: each one
// appears on the device, and its sequence number moved between reads.
//
// reads holds one entry per round of reading the device, each round keyed by the agent pod the
// slots were read through. Two rounds are needed, and that is the whole point of the parameter
// being a slice: every pod in a single round reads the same slots at the same moment, so one
// round yields identical sequence numbers for a node no matter how many pods it covers. Only a
// second round, taken after the agents have had time to write again, can show a counter move.
func validateSequenceNumbers(reads []map[string][]SlotData, expectedNodes int) error {
	// Collect every sequence number seen for each node, across every round and every pod.
	sequencesByNode := make(map[uint16][]uint64)

	for _, round := range reads {
		for _, slots := range round {
			for _, slot := range slots {
				if slot.HasData && slot.Sequence > 0 {
					sequencesByNode[slot.NodeID] = append(sequencesByNode[slot.NodeID], slot.Sequence)
				}
			}
		}
	}

	GinkgoWriter.Printf("Detected %d node IDs writing heartbeats over %d read(s)\n",
		len(sequencesByNode), len(reads))

	// Every node the test expects must have written a heartbeat at all.
	if len(sequencesByNode) < expectedNodes {
		return fmt.Errorf("expected heartbeats from %d nodes, but found %d",
			expectedNodes, len(sequencesByNode))
	}

	// And each of them must have advanced: the newest sequence seen for a node has to be higher
	// than the oldest, which can only happen if it wrote again between the two rounds.
	for nodeID, sequences := range sequencesByNode {
		lowest, highest := sequences[0], sequences[0]

		for _, sequence := range sequences {
			if sequence < lowest {
				lowest = sequence
			}

			if sequence > highest {
				highest = sequence
			}
		}

		GinkgoWriter.Printf("  Node ID %d: sequence %d -> %d over %d sample(s)\n",
			nodeID, lowest, highest, len(sequences))

		if highest <= lowest {
			return fmt.Errorf("node %d is not writing: its heartbeat sequence stayed at %d "+
				"across %d sample(s)", nodeID, lowest, len(sequences))
		}
	}

	GinkgoWriter.Printf("✓ All %d nodes advanced their heartbeat sequence\n", len(sequencesByNode))

	return nil
}

// setupBlockModeSBRC creates an SBRC backed by Ceph RBD in Block volume mode and waits for its
// agent DaemonSet to come up. It fails the suite when SNR is running (both operators claim the
// watchdog) and skips it when the cluster has no Ceph RBD StorageClass. The returned object is
// the created SBRC, for the caller to delete in AfterAll.
func setupBlockModeSBRC(sbrcName string) *unstructured.Unstructured {
	return setupBlockModeSBRCWithSpec(sbrcName, nil)
}

// setupBlockModeSBRCWithSpec is setupBlockModeSBRC with additionalSpec merged into the SBRC spec,
// for a test that has to tune the config it runs against - a shorter sbrTimeoutSeconds, say.
func setupBlockModeSBRCWithSpec(
	sbrcName string, additionalSpec map[string]interface{},
) *unstructured.Unstructured {
	failIfSNRRunning()

	By("Discovering Ceph RBD block storage class for Ceph RBD tests")

	// Skips the suite when the cluster has no Ceph RBD StorageClass.
	rbdStorageClass := discoverCephRBDStorageClass()

	By(fmt.Sprintf("Pre-cleaning stale SBRC %q if present", sbrcName))
	deleteSBRCAndWait(buildSBRC(sbrcName, map[string]interface{}{}), sbrcName)

	By(fmt.Sprintf("Creating StorageBasedRemediationConfig %q with Ceph RBD block storage", sbrcName))

	sbrc := buildSBRC(sbrcName, buildBlockModeSBRC(rbdStorageClass, additionalSpec))
	createErr := APIClient.Create(context.TODO(), sbrc)
	Expect(createErr).ToNot(HaveOccurred(),
		"StorageBasedRemediationConfig %q must be created for the Ceph RBD block storage test", sbrcName)

	By(fmt.Sprintf("Waiting for SBRC %q agent DaemonSet to be ready", sbrcName))
	waitForSBRCReady(sbrcName)

	return sbrc
}

// failIfSNRRunning fails the suite when Self Node Remediation is running, because SNR and SBR cannot
// both own the watchdog device at once. Shared by every Block-mode setup path.
func failIfSNRRunning() {
	By("Checking if Self Node Remediation (SNR) operator is running")

	snrRunning, snrPodCount := checkSNRRunning()
	if snrRunning {
		Fail(fmt.Sprintf("CANNOT RUN SBR BLOCK STORAGE TEST: Self Node Remediation (SNR) is running with %d pods.\n"+
			"SNR and SBR cannot both use the watchdog device simultaneously (resource conflict).\n"+
			"SBR requires detectOnlyMode: disabled (default) which needs exclusive watchdog access.\n\n"+
			"To run this test, you must scale down SNR:\n"+
			"  oc scale daemonset self-node-remediation-ds -n openshift-workload-availability --replicas=0\n\n"+
			"Alternatively, configure SBR with detectOnlyMode: enabled (but this changes test behavior).",
			snrPodCount))
	}

	GinkgoWriter.Printf("✓ SNR is not running - SBR can safely access watchdog device\n")
}

// waitForBlockInitJobSucceeded blocks until the block device-init Job owned by the given SBRC reports
// completion. The operator runs one init Job per SBRC and gates the agent DaemonSet on it, so waiting
// on the Job directly turns an init failure into an explicit "init Job failed" error rather than an
// opaque "DaemonSet not ready" timeout.
//
// The Job is matched by owner UID, not by name: the pre-clean waits for the old SBRC to be deleted but
// not for its Job, and a stale succeeded Job from a previous run shares the name. Completion is read
// from the Job's Complete/Failed conditions rather than the Succeeded/Failed counts, because the
// operator sets BackoffLimit: 1, so a Job that fails one pod and succeeds on retry ends with both
// Failed=1 and Succeeded=1 — a healthy outcome the counts alone cannot distinguish from a real failure.
func waitForBlockInitJobSucceeded(sbrcName string, ownerUID types.UID) {
	Eventually(func() error {
		jobList, listErr := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(
			context.TODO(), metav1.ListOptions{})
		if listErr != nil {
			return fmt.Errorf("list Jobs in %s: %w", medik8sparams.OperatorNs, listErr)
		}

		for idx := range jobList.Items {
			job := &jobList.Items[idx]
			if !ownedBy(job.OwnerReferences, ownerUID) {
				continue
			}

			for _, cond := range job.Status.Conditions {
				if cond.Status != corev1.ConditionTrue {
					continue
				}

				if cond.Type == batchv1.JobComplete {
					return nil
				}

				if cond.Type == batchv1.JobFailed {
					return fmt.Errorf("block init Job %s failed: %s", job.Name, cond.Message)
				}
			}

			return fmt.Errorf("block init Job %s not complete yet (active=%d)", job.Name, job.Status.Active)
		}

		return fmt.Errorf("no block init Job owned by SBRC %s yet", sbrcName)
	}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"Block device-init Job for SBRC %q must complete successfully", sbrcName)
}

// ownedBy reports whether any of the owner references carries the given UID.
func ownedBy(owners []metav1.OwnerReference, ownerUID types.UID) bool {
	for _, owner := range owners {
		if owner.UID == ownerUID {
			return true
		}
	}

	return false
}

// deleteSBRCAndWait deletes an SBRC and blocks until it is gone. Deletion failures are logged
// rather than asserted so teardown never masks the test result; the wait itself is asserted so
// a stuck SBRC does not silently leak into the next suite.
func deleteSBRCAndWait(sbrc *unstructured.Unstructured, sbrcName string) {
	if deleteErr := APIClient.Delete(context.TODO(), sbrc); deleteErr != nil {
		if k8serrors.IsNotFound(deleteErr) {
			return
		}

		GinkgoWriter.Printf("Warning: delete SBRC %s: %v\n", sbrcName, deleteErr)
	}

	Eventually(func() error {
		getErr := APIClient.Get(context.TODO(),
			types.NamespacedName{Name: sbrcName, Namespace: medik8sparams.OperatorNs},
			sbrc.DeepCopy())

		if k8serrors.IsNotFound(getErr) {
			return nil
		}

		if getErr != nil {
			return getErr
		}

		return fmt.Errorf("SBRC %s still present", sbrcName)
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"SBRC %q must be gone", sbrcName)
}

// blockSuperblock is the parsed sector-0 header of a Block-mode SBR device. CRC and ComputedCRC
// are both kept so a caller can assert the stored checksum is the right one rather than being
// handed a parse error for a device it is trying to diagnose.
type blockSuperblock struct {
	Magic              string
	Version            uint16
	NodeMapAOffset     int64
	NodeMapALength     int64
	NodeMapBOffset     int64
	NodeMapBLength     int64
	HeartbeatRegOffset int64
	HeartbeatRegLength int64
	FenceRegOffset     int64
	FenceRegLength     int64
	Flags              uint32
	CRC                uint32
	ComputedCRC        uint32
}

// blockNodeMapEntry is one node's slot assignment, as the agent writes it into the node map JSON.
type blockNodeMapEntry struct {
	NodeName string `json:"node_name"`
	NodeID   uint16 `json:"node_id"`
}

// blockNodeMap is the subset of sbdprotocol.NodeMapTable this suite reads back. Entries is keyed
// by node name; NodeUsage is the reverse index, keyed by node ID rendered as a JSON object key.
type blockNodeMap struct {
	Magic      [8]byte                      `json:"magic"`
	Version    uint64                       `json:"version"`
	Entries    map[string]blockNodeMapEntry `json:"entries"`
	NodeUsage  map[string]string            `json:"node_usage"`
	Generation uint64                       `json:"-"`
}

// readSuperblockFromPod reads sector 0 through an agent pod and parses the superblock out of it.
func readSuperblockFromPod(podName string) (*blockSuperblock, []byte, error) {
	data, readErr := readDeviceBlocksFromPod(podName, superblockBlock, 1)
	if readErr != nil {
		return nil, nil, readErr
	}

	superblock, parseErr := parseSuperblock(data)
	if parseErr != nil {
		return nil, data[:superblockTotalSize], parseErr
	}

	return superblock, data[:superblockTotalSize], nil
}

// parseSuperblock decodes the 80-byte superblock header. It only fails on a short read: a wrong
// magic, version or checksum is reported through the returned struct, for the caller to assert on.
func parseSuperblock(data []byte) (*blockSuperblock, error) {
	if len(data) < superblockTotalSize {
		return nil, fmt.Errorf("superblock too short: got %d bytes, want %d",
			len(data), superblockTotalSize)
	}

	fields := make([]int64, superblockRegionFields)
	for idx := range fields {
		start := 8 + idx*8
		fields[idx] = int64(binary.LittleEndian.Uint64(data[start : start+8]))
	}

	return &blockSuperblock{
		Magic:              string(data[0:6]),
		Version:            binary.LittleEndian.Uint16(data[6:8]),
		NodeMapAOffset:     fields[0],
		NodeMapALength:     fields[1],
		NodeMapBOffset:     fields[2],
		NodeMapBLength:     fields[3],
		HeartbeatRegOffset: fields[4],
		HeartbeatRegLength: fields[5],
		FenceRegOffset:     fields[6],
		FenceRegLength:     fields[7],
		Flags:              binary.LittleEndian.Uint32(data[72:76]),
		CRC:                binary.LittleEndian.Uint32(data[76:80]),
		ComputedCRC:        crc32.ChecksumIEEE(data[:superblockDataSize]),
	}, nil
}

// readNodeMapFromPod reads the node map through an agent pod and returns the live buffer: the
// valid one with the highest generation, exactly as BlockNodeMapStore.Load picks it. The raw JSON
// payload is returned alongside, for test artifacts.
//
// The read is retried because the agents rewrite these buffers continuously - generations were
// seen advancing by four between two consecutive dumps - so a single attempt can catch a buffer
// mid-write and fail its checksum. Each attempt re-reads both buffers; an attempt counts as
// successful as soon as one of them validates, which is also what the agents themselves rely on.
func readNodeMapFromPod(podName string) (*blockNodeMap, string, error) {
	var lastErr error

	for attempt := 1; attempt <= sbrparams.BlockNodeMapReadAttempts; attempt++ {
		nodeMap, jsonPayload, err := readNodeMapOnce(podName)
		if err == nil {
			return nodeMap, jsonPayload, nil
		}

		lastErr = err
		GinkgoWriter.Printf("Node map read attempt %d/%d failed: %v\n",
			attempt, sbrparams.BlockNodeMapReadAttempts, err)
	}

	return nil, "", fmt.Errorf("node map unreadable after %d attempts: %w",
		sbrparams.BlockNodeMapReadAttempts, lastErr)
}

// readNodeMapOnce makes one attempt at reading the node map. Each buffer is read in a single dd
// read, so the 64 KB a checksum covers comes from one point in time rather than sixteen.
func readNodeMapOnce(podName string) (*blockNodeMap, string, error) {
	var (
		bestPayload []byte
		bestGen     uint64
		bufferErrs  []string
	)

	for _, byteOffset := range []int{nodeMapABlock * blockSlotSize, nodeMapBBlock * blockSlotSize} {
		raw, readErr := readDeviceRegionFromPod(podName, byteOffset, nodeMapRegionSize)
		if readErr != nil {
			return nil, "", readErr
		}

		generation, payload, parseErr := parseNodeMapBuffer(raw)
		if parseErr != nil {
			bufferErrs = append(bufferErrs, fmt.Sprintf("buffer at byte %d: %v", byteOffset, parseErr))

			continue
		}

		if generation > bestGen {
			bestGen, bestPayload = generation, payload
		}
	}

	if bestPayload == nil {
		return nil, "", fmt.Errorf("no valid node map buffer on the device (%s)",
			strings.Join(bufferErrs, "; "))
	}

	nodeMap, jsonPayload, parseErr := parseNodeMapPayload(bestPayload)
	if parseErr != nil {
		return nil, jsonPayload, parseErr
	}

	nodeMap.Generation = bestGen

	return nodeMap, jsonPayload, nil
}

// parseNodeMapBuffer validates one 64 KB node map buffer and returns its generation and payload.
// Generation 0 means the buffer was never written, which is not an error on a device whose
// writers have only ever used the other buffer.
func parseNodeMapBuffer(raw []byte) (uint64, []byte, error) {
	if len(raw) < nodeMapRegionSize {
		return 0, nil, fmt.Errorf("node map buffer too short: got %d bytes, want %d",
			len(raw), nodeMapRegionSize)
	}

	generation := binary.LittleEndian.Uint64(raw[0:8])
	storedCRC := binary.LittleEndian.Uint32(raw[nodeMapCRCOffset : nodeMapCRCOffset+4])
	if computed := crc32.ChecksumIEEE(raw[:nodeMapCRCOffset]); computed != storedCRC {
		return generation, nil, fmt.Errorf("node map envelope CRC mismatch: stored %d, computed %d",
			storedCRC, computed)
	}

	if generation == 0 {
		return 0, nil, errors.New("node map buffer is unused (generation 0)")
	}

	payloadLen := int(binary.LittleEndian.Uint32(
		raw[nodeMapPayloadLenOffset : nodeMapPayloadLenOffset+4]))
	if payloadLen < nodeMapJSONCRCSize || nodeMapPayloadOffset+payloadLen > nodeMapCRCOffset {
		return generation, nil, fmt.Errorf("node map payload length %d is out of range", payloadLen)
	}

	return generation, raw[nodeMapPayloadOffset : nodeMapPayloadOffset+payloadLen], nil
}

// parseNodeMapPayload checks the CRC32 that NodeMapTable.Marshal prepends to its JSON, then
// decodes the table. The JSON is returned even when decoding fails, so a failure message can show
// what was actually on the device.
func parseNodeMapPayload(payload []byte) (*blockNodeMap, string, error) {
	storedCRC := binary.LittleEndian.Uint32(payload[:nodeMapJSONCRCSize])
	jsonBytes := payload[nodeMapJSONCRCSize:]

	if computed := crc32.ChecksumIEEE(jsonBytes); computed != storedCRC {
		return nil, string(jsonBytes), fmt.Errorf("node map JSON CRC mismatch: stored %d, computed %d",
			storedCRC, computed)
	}

	nodeMap := &blockNodeMap{}
	if unmarshalErr := json.Unmarshal(jsonBytes, nodeMap); unmarshalErr != nil {
		return nil, string(jsonBytes), fmt.Errorf("decode node map JSON: %w", unmarshalErr)
	}

	if magic := string(nodeMap.Magic[:]); magic != nodeMapMagic {
		return nodeMap, string(jsonBytes), fmt.Errorf("node map magic is %q, want %q",
			magic, nodeMapMagic)
	}

	return nodeMap, string(jsonBytes), nil
}

// readFenceSlotsFromPod reads the whole fence region through an agent pod and returns the slots
// that carry a fence message, keyed by slot index. A slot with no valid message is simply absent:
// on a healthy device that is every slot.
func readFenceSlotsFromPod(podName string) (map[int]FenceSlot, error) {
	data, readErr := readDeviceBlocksFromPod(podName, fenceRegionBlock, blockMaxNodes)
	if readErr != nil {
		return nil, readErr
	}

	return parseFenceSlots(data), nil
}

// parseFenceSlots parses raw fence-region bytes. A zero-filled slot is a healthy, never-written
// slot and is left out of the map. A slot that holds data but is not a valid SBD fence message
// (bad magic, wrong message type, or a header checksum mismatch) is recorded with Corrupt set, so
// the inspection can fail on it rather than mistake it for empty.
func parseFenceSlots(data []byte) map[int]FenceSlot {
	slots := make(map[int]FenceSlot)

	for slotIdx := 0; slotIdx < len(data)/blockSlotSize && slotIdx < blockMaxNodes; slotIdx++ {
		slotBytes := validSBDHeader(data, slotIdx, sbdFenceMsgType, sbdFenceMsgSize)

		if slotBytes == nil {
			if !slotRegionIsZero(data, slotIdx) {
				slots[slotIdx] = FenceSlot{SlotIndex: slotIdx, Corrupt: true}
			}

			continue
		}

		slots[slotIdx] = FenceSlot{
			SlotIndex:    slotIdx,
			WriterNodeID: binary.LittleEndian.Uint16(slotBytes[11:13]),
			TargetNodeID: binary.LittleEndian.Uint16(slotBytes[33:35]),
			Reason:       slotBytes[35],
		}
	}

	return slots
}

// slotRegionIsZero reports whether the whole 4 KB slot at slotIdx is zero-filled, which is what a
// healthy slot no agent has ever written looks like.
func slotRegionIsZero(data []byte, slotIdx int) bool {
	start := slotIdx * blockSlotSize
	end := start + blockSlotSize

	if end > len(data) {
		end = len(data)
	}

	for _, b := range data[start:end] {
		if b != 0 {
			return false
		}
	}

	return true
}
