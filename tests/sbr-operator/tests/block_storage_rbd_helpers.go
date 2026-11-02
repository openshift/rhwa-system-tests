package tests

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
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

// errSharedStoragePVCNotFound is returned by sbrcSharedStoragePVC when the SBRC owns no PVC. It is
// a sentinel rather than a plain error so waitForSBRCPVCGone can treat "absent" as success without
// also swallowing a list failure, which would report a leaked PVC as cleaned up.
var errSharedStoragePVCNotFound = errors.New("no shared-storage PVC owned by the StorageBasedRemediationConfig")

// sbrcSharedStoragePVC returns the shared-storage PVC the operator provisioned for the SBRC with
// the given UID, or errSharedStoragePVCNotFound when there is none. The PVC is located by owner
// reference rather than by name, so the test never has to mirror the operator's naming scheme and a
// stale PVC left by an earlier run under the same SBRC name is not mistaken for this one.
func sbrcSharedStoragePVC(ownerUID types.UID) (*corev1.PersistentVolumeClaim, error) {
	pvcList, listErr := APIClient.PersistentVolumeClaims(medik8sparams.OperatorNs).List(
		context.TODO(), metav1.ListOptions{})
	if listErr != nil {
		return nil, fmt.Errorf("list PVCs in %s: %w", medik8sparams.OperatorNs, listErr)
	}

	for idx := range pvcList.Items {
		if ownedBy(pvcList.Items[idx].OwnerReferences, ownerUID) {
			return &pvcList.Items[idx], nil
		}
	}

	return nil, errSharedStoragePVCNotFound
}

// waitForSBRCPVCBound blocks until the SBRC's shared-storage PVC exists and returns it. The PVC is
// created on the first reconcile, so a test that inspects it right after creating the SBRC can
// otherwise race the controller.
func waitForSBRCPVCBound(sbrcName string, ownerUID types.UID) *corev1.PersistentVolumeClaim {
	var pvc *corev1.PersistentVolumeClaim

	Eventually(func() error {
		found, findErr := sbrcSharedStoragePVC(ownerUID)
		if findErr != nil {
			return fmt.Errorf("SBRC %s: %w", sbrcName, findErr)
		}

		pvc = found

		return nil
	}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"Shared-storage PVC for SBRC %q must be created by the operator", sbrcName)

	return pvc
}

// waitForSBRCPVCGone blocks until the SBRC's shared-storage PVC has been garbage-collected. The
// operator patches the backing PV's reclaim policy to Delete before dropping its finalizer, so a
// PVC that outlives its SBRC means the teardown path did not complete.
func waitForSBRCPVCGone(sbrcName string, ownerUID types.UID) {
	Eventually(func() error {
		pvc, findErr := sbrcSharedStoragePVC(ownerUID)
		if errors.Is(findErr, errSharedStoragePVCNotFound) {
			return nil
		}

		if findErr != nil {
			return findErr
		}

		return fmt.Errorf("PVC %s owned by SBRC %s still present (phase %s)",
			pvc.Name, sbrcName, pvc.Status.Phase)
	}, sbrparams.SBRCPVCGCTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"Shared-storage PVC of SBRC %q must be garbage-collected after the SBRC is deleted", sbrcName)
}

// sbrcOwnedJob returns the device-init Job in the operator namespace owned by the given SBRC, or an
// error when there is none. Matching on the owner keeps a same-named Job left by a previous run out
// of the result. It is the only Job an SBRC owns while it is alive, so the first match is it.
func sbrcOwnedJob(ownerUID types.UID) (*batchv1.Job, error) {
	jobList, listErr := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(
		context.TODO(), metav1.ListOptions{})
	if listErr != nil {
		return nil, fmt.Errorf("list Jobs in %s: %w", medik8sparams.OperatorNs, listErr)
	}

	for idx := range jobList.Items {
		if ownedBy(jobList.Items[idx].OwnerReferences, ownerUID) {
			return &jobList.Items[idx], nil
		}
	}

	return nil, fmt.Errorf("no Job owned by UID %s in %s", ownerUID, medik8sparams.OperatorNs)
}

// blockInitJobCompletionTime returns when the SBRC's Block device-init Job finished. It prefers the
// Job's CompletionTime and falls back to the Complete condition's transition time, because a Job
// that the API server has marked Complete may not carry CompletionTime yet.
//
// Only call this once waitForBlockInitJobSucceeded has returned: the Job is TTL-collected after it
// finishes, so the timestamp has to be read while it is still there.
func blockInitJobCompletionTime(sbrcName string, ownerUID types.UID) (time.Time, error) {
	job, findErr := sbrcOwnedJob(ownerUID)
	if findErr != nil {
		return time.Time{}, fmt.Errorf("block init Job for SBRC %s: %w", sbrcName, findErr)
	}

	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time, nil
	}

	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return cond.LastTransitionTime.Time, nil
		}
	}

	return time.Time{}, fmt.Errorf("block init Job %s carries no completion timestamp", job.Name)
}

// sbrcNodeMapCleanupJobs returns the node map cleanup Jobs (RHWA-1056) the operator ran for the
// named SBRC. Block mode keeps its node map inside the device, not in a filesystem file, so the
// operator skips that Job and this must stay empty.
//
// The Jobs are selected by the labels the operator stamps on them rather than by name: the Job uses
// a GenerateName, so its name carries a random suffix. They are not selected by owner reference
// either - the Job is created while the SBRC is being deleted, so by the time a test could look the
// owner is already gone.
func sbrcNodeMapCleanupJobs(sbrcName string) ([]batchv1.Job, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s",
		sbrparams.SBRCleanupJobComponentLabelKey, sbrparams.SBRCleanupJobComponentLabelValue,
		sbrparams.SBRCConfigLabelKey, sbrcName)
	jobList, listErr := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(
		context.TODO(), metav1.ListOptions{LabelSelector: selector})
	if listErr != nil {
		return nil, fmt.Errorf("list Jobs matching %q in %s: %w",
			selector, medik8sparams.OperatorNs, listErr)
	}

	return jobList.Items, nil
}

// deleteSBRCAssertingNoCleanupJob deletes an SBRC and watches for a node map cleanup Job from the
// moment the delete is issued until the SBRC is gone, then reports any Job it saw.
//
// The watch has to start with the delete, not after it: the operator creates that Job during the
// finalizer drain and it is TTL-collected afterwards, so a check that only runs once the SBRC has
// disappeared can step straight over one that ran. The poll is deliberately tighter than the
// suite's usual interval for the same reason.
func deleteSBRCAssertingNoCleanupJob(sbrc *unstructured.Unstructured, sbrcName string) {
	if deleteErr := APIClient.Delete(context.TODO(), sbrc); deleteErr != nil &&
		!k8serrors.IsNotFound(deleteErr) {
		GinkgoWriter.Printf("Warning: delete SBRC %s: %v\n", sbrcName, deleteErr)
	}

	var (
		seenCleanupJob string
		cleanReads     int
		lastListErr    error
	)

	Eventually(func() error {
		cleanupJobs, listErr := sbrcNodeMapCleanupJobs(sbrcName)

		switch {
		case listErr != nil:
			// Never let a failed look count as "no Job seen": that is the whole assertion.
			lastListErr = listErr
		case len(cleanupJobs) > 0:
			if seenCleanupJob == "" {
				seenCleanupJob = cleanupJobs[0].Name
			}

			cleanReads++
		default:
			cleanReads++
		}

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
	}, medik8sparams.DefaultTimeout, sbrparams.CleanupJobWatchInterval).Should(Succeed(),
		"SBRC %q must be gone", sbrcName)

	Expect(seenCleanupJob).To(BeEmpty(),
		"Node map cleanup Job %q was created while deleting Block-mode SBRC %q: Block mode has no "+
			"filesystem-level node map file to wipe, so that Job must never run",
		seenCleanupJob, sbrcName)

	// A window the Jobs could never be listed in proves nothing about what ran in it.
	Expect(cleanReads).To(BeNumerically(">", 0),
		"The Jobs in %s could not be listed even once while SBRC %q was being deleted, so the "+
			"absence of a node map cleanup Job was never established: %v",
		medik8sparams.OperatorNs, sbrcName, lastListErr)
}

// sbrcAgentDaemonSet returns the agent DaemonSet the operator created for the named SBRC.
func sbrcAgentDaemonSet(sbrcName string) (*appsv1.DaemonSet, error) {
	dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrcName
	agentDS, getErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
		context.TODO(), dsName, metav1.GetOptions{})
	if getErr != nil {
		return nil, fmt.Errorf("get DaemonSet %s: %w", dsName, getErr)
	}

	return agentDS, nil
}

// agentContainerOf returns the sbr-agent container of an agent DaemonSet's pod template.
func agentContainerOf(agentDS *appsv1.DaemonSet) (*corev1.Container, error) {
	for idx := range agentDS.Spec.Template.Spec.Containers {
		if agentDS.Spec.Template.Spec.Containers[idx].Name == sbrparams.AgentContainerName {
			return &agentDS.Spec.Template.Spec.Containers[idx], nil
		}
	}

	return nil, fmt.Errorf("DaemonSet %s has no %q container",
		agentDS.Name, sbrparams.AgentContainerName)
}

// podVolumeByName returns the named volume of a DaemonSet's pod template.
func podVolumeByName(agentDS *appsv1.DaemonSet, volumeName string) (*corev1.Volume, error) {
	for idx := range agentDS.Spec.Template.Spec.Volumes {
		if agentDS.Spec.Template.Spec.Volumes[idx].Name == volumeName {
			return &agentDS.Spec.Template.Spec.Volumes[idx], nil
		}
	}

	return nil, fmt.Errorf("DaemonSet %s has no volume named %q", agentDS.Name, volumeName)
}

// containerArgValue returns the value of the first --flag=value argument carrying flagName, and
// reports whether it was present at all, so a caller can tell "flag absent" from "flag empty".
func containerArgValue(container *corev1.Container, flagName string) (string, bool) {
	prefix := flagName + "="
	for _, arg := range container.Args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
	}

	return "", false
}

// assertBlockModeSharedStorageAttached proves the agent DaemonSet attaches the shared storage as a
// raw device: a volumeDevice at sbrBlockDevicePath backed by the shared-storage PVC, and no
// volumeMount for the same volume. Mounting it as a filesystem instead would mean the agent is not
// in Block mode at all.
func assertBlockModeSharedStorageAttached(agentDS *appsv1.DaemonSet, pvcName string) {
	agentContainer, containerErr := agentContainerOf(agentDS)
	Expect(containerErr).ToNot(HaveOccurred())
	Expect(agentContainer.VolumeDevices).To(ContainElement(corev1.VolumeDevice{
		Name:       sbrparams.SharedStorageVolumeName,
		DevicePath: sbrBlockDevicePath,
	}), "Block-mode agent container of DaemonSet %s must attach the shared storage as a raw device "+
		"at %s; volumeDevices were %+v", agentDS.Name, sbrBlockDevicePath, agentContainer.VolumeDevices)

	for _, mount := range agentContainer.VolumeMounts {
		Expect(mount.Name).ToNot(Equal(sbrparams.SharedStorageVolumeName),
			"Block-mode agent container of DaemonSet %s must not mount the shared storage as a "+
				"filesystem, but volume %q is mounted at %s",
			agentDS.Name, mount.Name, mount.MountPath)
	}

	sharedVolume, volumeErr := podVolumeByName(agentDS, sbrparams.SharedStorageVolumeName)
	Expect(volumeErr).ToNot(HaveOccurred())
	Expect(sharedVolume.PersistentVolumeClaim).ToNot(BeNil(),
		"Volume %q of DaemonSet %s must be backed by a PVC",
		sbrparams.SharedStorageVolumeName, agentDS.Name)
	Expect(sharedVolume.PersistentVolumeClaim.ClaimName).To(Equal(pvcName),
		"Volume %q of DaemonSet %s must be backed by the SBRC's own shared-storage PVC",
		sbrparams.SharedStorageVolumeName, agentDS.Name)
}

// assertBlockModeAgentArgs proves the agent is told to run against the raw device with file locking
// off. A raw device bypasses the cluster lock manager, so flock would degrade to host-local
// semantics and the node map would lose its mutual exclusion.
func assertBlockModeAgentArgs(agentDS *appsv1.DaemonSet) {
	agentContainer, containerErr := agentContainerOf(agentDS)
	Expect(containerErr).ToNot(HaveOccurred())
	devicePath, devicePresent := containerArgValue(agentContainer, sbrparams.AgentSBRDeviceFlag)
	Expect(devicePresent).To(BeTrue(),
		"Block-mode agent args of DaemonSet %s must carry %s; args were %v",
		agentDS.Name, sbrparams.AgentSBRDeviceFlag, agentContainer.Args)
	Expect(devicePath).To(Equal(sbrBlockDevicePath),
		"Block-mode agent of DaemonSet %s must be pointed at the raw device", agentDS.Name)
	locking, lockingPresent := containerArgValue(agentContainer, sbrparams.AgentFileLockingFlag)
	Expect(lockingPresent).To(BeTrue(),
		"Block-mode agent args of DaemonSet %s must carry %s; args were %v",
		agentDS.Name, sbrparams.AgentFileLockingFlag, agentContainer.Args)
	Expect(locking).To(Equal("false"),
		"Block-mode agent of DaemonSet %s must run with file locking disabled", agentDS.Name)
}

// assertBlockModeWatchdogMount proves the watchdog was moved out of /dev. A bind mount at /dev makes
// the OCI runtime skip creating the CSI-mapped block device node, so Block mode mounts only the
// watchdog's parent directory at BlockModeHostDevMountPath and points the agent under it.
func assertBlockModeWatchdogMount(agentDS *appsv1.DaemonSet, hostWatchdogPath string) {
	agentContainer, containerErr := agentContainerOf(agentDS)
	Expect(containerErr).ToNot(HaveOccurred())
	hostDevVolume, volumeErr := podVolumeByName(agentDS, "host-dev")
	Expect(volumeErr).ToNot(HaveOccurred(),
		"Block-mode DaemonSet %s must carry a host-dev volume for the watchdog's parent directory",
		agentDS.Name)
	Expect(hostDevVolume.HostPath).ToNot(BeNil(),
		"Volume host-dev of DaemonSet %s must be a hostPath volume", agentDS.Name)
	Expect(hostDevVolume.HostPath.Path).To(Equal(filepath.Dir(hostWatchdogPath)),
		"Volume host-dev of DaemonSet %s must expose the watchdog's parent directory", agentDS.Name)

	var hostDevMountPath string

	for _, mount := range agentContainer.VolumeMounts {
		Expect(mount.MountPath).ToNot(Equal("/dev"),
			"Block-mode agent container of DaemonSet %s must not mount anything at /dev: a bind "+
				"mount there makes the runtime skip the CSI block device node (volume %q)",
			agentDS.Name, mount.Name)

		if mount.Name == "host-dev" {
			hostDevMountPath = mount.MountPath
		}
	}

	Expect(hostDevMountPath).To(Equal(sbrparams.BlockModeHostDevMountPath),
		"Block-mode agent container of DaemonSet %s must mount host-dev at %s",
		agentDS.Name, sbrparams.BlockModeHostDevMountPath)
	watchdogArg, watchdogPresent := containerArgValue(agentContainer, sbrparams.AgentWatchdogPathFlag)
	Expect(watchdogPresent).To(BeTrue(),
		"Agent args of DaemonSet %s must carry %s; args were %v",
		agentDS.Name, sbrparams.AgentWatchdogPathFlag, agentContainer.Args)
	Expect(watchdogArg).To(Equal(
		filepath.Join(sbrparams.BlockModeHostDevMountPath, filepath.Base(hostWatchdogPath))),
		"Block-mode agent of DaemonSet %s must resolve the watchdog under %s, not on the host path",
		agentDS.Name, sbrparams.BlockModeHostDevMountPath)
}

// patchSBRCSpec merge-patches spec fields onto an existing SBRC and returns the API error, so a
// caller can assert either that the patch lands or that admission rejects it.
func patchSBRCSpec(sbrcName string, specFields map[string]interface{}) error {
	payload, marshalErr := json.Marshal(map[string]interface{}{"spec": specFields})
	if marshalErr != nil {
		return fmt.Errorf("marshal patch for SBRC %s: %w", sbrcName, marshalErr)
	}

	return APIClient.Patch(context.TODO(),
		buildSBRC(sbrcName, map[string]interface{}{}),
		runtimeclient.RawPatch(types.MergePatchType, payload))
}

// waitForDaemonSetRollout blocks until the named DaemonSet has moved past prePatchGeneration, has
// reconciled that generation, and has every pod replaced with the new revision, Ready and
// available. Used after an SBRC spec change to prove the operator pushed it down to all of the
// agents rather than leaving some of them on the old configuration.
//
// The Filesystem lifecycle test (sbrc_lifecycle.go) still has this inline; it could call this.
func waitForDaemonSetRollout(dsName string, prePatchGeneration int64) {
	Eventually(func() error {
		agentDS, getErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
			context.TODO(), dsName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("DaemonSet %s not found: %w", dsName, getErr)
		}

		if agentDS.Generation <= prePatchGeneration {
			return fmt.Errorf("DaemonSet %s generation not advanced (current: %d, pre-patch: %d)",
				dsName, agentDS.Generation, prePatchGeneration)
		}

		if agentDS.Status.ObservedGeneration < agentDS.Generation {
			return fmt.Errorf("DaemonSet %s not yet reconciled (observed: %d, current: %d)",
				dsName, agentDS.Status.ObservedGeneration, agentDS.Generation)
		}

		desired := agentDS.Status.DesiredNumberScheduled
		if desired == 0 {
			return fmt.Errorf("DaemonSet %s schedules no pods at all", dsName)
		}

		// Every agent has to be carrying the new spec, not just the first one to restart: a
		// half-rolled DaemonSet leaves some agents running the old configuration.
		if agentDS.Status.UpdatedNumberScheduled < desired {
			return fmt.Errorf("DaemonSet %s rollout incomplete: %d/%d pods on the new revision",
				dsName, agentDS.Status.UpdatedNumberScheduled, desired)
		}

		if agentDS.Status.NumberReady < desired {
			return fmt.Errorf("DaemonSet %s: %d/%d pods ready after rollout",
				dsName, agentDS.Status.NumberReady, desired)
		}

		if agentDS.Status.NumberUnavailable > 0 {
			return fmt.Errorf("DaemonSet %s still has %d unavailable pod(s)",
				dsName, agentDS.Status.NumberUnavailable)
		}

		return nil
	}, 3*sbrparams.SBRCLifecyclePatchedTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"DaemonSet %s must complete its rollout after the SBRC is patched", dsName)
}

// sbrcAdmissionWebhookRegistered reports whether the cluster has a validating admission webhook
// covering StorageBasedRemediationConfig objects.
//
// Some of the SBRC spec rules are enforced in two places and some in only one. Where the controller
// re-checks a rule at reconcile time, a test can assert the outcome either way. Immutability of
// sharedStorageVolumeMode is webhook-only, so a test asserting that an update is refused has to
// know whether anything is there to refuse it.
func sbrcAdmissionWebhookRegistered() bool {
	configs, listErr := APIClient.K8sClient.AdmissionregistrationV1().
		ValidatingWebhookConfigurations().List(context.TODO(), metav1.ListOptions{})
	if listErr != nil {
		GinkgoWriter.Printf("Warning: cannot list ValidatingWebhookConfigurations: %v\n", listErr)

		return false
	}

	for cfgIdx := range configs.Items {
		for _, hook := range configs.Items[cfgIdx].Webhooks {
			for _, rule := range hook.Rules {
				if slices.Contains(rule.APIGroups, sbrparams.CRDGroup) &&
					slices.Contains(rule.Resources, sbrparams.SBRCResourcePlural) {
					GinkgoWriter.Printf("StorageBasedRemediationConfig admission webhook %q is registered\n",
						hook.Name)

					return true
				}
			}
		}
	}

	GinkgoWriter.Printf("No validating admission webhook covers %s.%s on this cluster\n",
		sbrparams.SBRCResourcePlural, sbrparams.CRDGroup)

	return false
}

// assertInvalidSBRCNeverBecomesOperational proves an SBRC whose spec is invalid never produces
// agents, through whichever layer is actually enforcing the rule on this cluster.
//
// A spec rule that both the admission webhook and the controller enforce has two correct outcomes,
// and which one applies is decided by whether the webhook is registered, not by what the create
// call happens to return:
//
//   - webhook registered: the create must be refused, and refused over wantDenialSubstr. An
//     AlreadyExists from an SBRC a crashed run left behind, or a timeout, is not a rejection and
//     must not be mistaken for one - nothing was validated in those cases.
//   - webhook not registered: the create must succeed, because there is nothing to refuse it. The
//     controller then refuses it at reconcile, emitting a ValidationError Warning and never
//     building a DaemonSet.
//
// The caller owns deleting the CR: it exists afterwards in the second case.
func assertInvalidSBRCNeverBecomesOperational(
	sbrc *unstructured.Unstructured, sbrcName, wantDenialSubstr string,
) {
	createErr := APIClient.Create(context.TODO(), sbrc)

	if sbrcAdmissionWebhookRegistered() {
		expectAdmissionDenial(createErr, wantDenialSubstr,
			fmt.Sprintf("The validating admission webhook is registered, so SBRC %q must be refused "+
				"at creation", sbrcName))
		GinkgoWriter.Printf("SBRC %q was refused at admission: %v\n", sbrcName, createErr)

		return
	}

	Expect(createErr).ToNot(HaveOccurred(),
		"No validating admission webhook covers %s on this cluster, so SBRC %q must be admitted and "+
			"refused by the controller instead", sbrparams.SBRCResourcePlural, sbrcName)
	GinkgoWriter.Printf("SBRC %q was admitted (no validating webhook); asserting the controller "+
		"refuses it at reconcile instead\n", sbrcName)

	By(fmt.Sprintf("Waiting for a %s event on the admitted SBRC %q",
		sbrparams.SBRCValidationErrorReason, sbrcName))

	eventErr := helpers.WaitForEvents(context.TODO(), EventsClient,
		helpers.InvolvedObjectRef{
			Kind:      sbrparams.SBRCKind,
			Name:      sbrcName,
			Namespace: medik8sparams.OperatorNs,
			UID:       string(sbrc.GetUID()),
		},
		[]helpers.EventExpectation{{
			Reason: sbrparams.SBRCValidationErrorReason,
			Type:   corev1.EventTypeWarning,
		}},
		medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval)
	Expect(eventErr).ToNot(HaveOccurred(),
		"The controller must emit a %s Warning for the invalid SBRC %q it was handed",
		sbrparams.SBRCValidationErrorReason, sbrcName)

	By(fmt.Sprintf("Asserting no agent DaemonSet is ever created for the invalid SBRC %q", sbrcName))

	dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrcName

	Consistently(func() error {
		_, getErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
			context.TODO(), dsName, metav1.GetOptions{})
		if k8serrors.IsNotFound(getErr) {
			return nil
		}

		if getErr != nil {
			return getErr
		}

		return fmt.Errorf("DaemonSet %s was created for the invalid SBRC %s", dsName, sbrcName)
	}, sbrparams.NoNewDaemonSetCheckDuration, sbrparams.NoNewDaemonSetCheckInterval).Should(Succeed(),
		"An SBRC that fails spec validation must never get agents")
}

// expectAdmissionDenial asserts that err is a validating admission webhook turning a request down
// over wantFieldSubstr, and not some other way for an API call to fail.
//
// A bare "an error happened" check is not enough for a negative admission assertion: a timeout, a
// missing RBAC permission or an AlreadyExists from a crashed earlier run would all satisfy it while
// proving nothing about the rule under test. A denial arrives as Forbidden or Invalid and carries
// the webhook's own message, so both are checked.
func expectAdmissionDenial(err error, wantFieldSubstr, description string) {
	Expect(err).To(HaveOccurred(), "%s", description)
	Expect(k8serrors.IsForbidden(err) || k8serrors.IsInvalid(err)).To(BeTrue(),
		"%s: expected an admission denial (Forbidden or Invalid) but the API call failed another "+
			"way, so nothing was validated: %v", description, err)
	Expect(err.Error()).To(ContainSubstring(wantFieldSubstr),
		"%s: the request was turned down, but not over %q, so the denial is not the one under "+
			"test: %v", description, wantFieldSubstr, err)
}

// sbrcVolumeMode reads sharedStorageVolumeMode straight off the live SBRC. An unset field comes
// back empty rather than resolved to its default, so a caller asserting the stored value sees what
// is actually on the object.
func sbrcVolumeMode(sbrcName string) (string, error) {
	sbrc := buildSBRC(sbrcName, map[string]interface{}{})
	if getErr := APIClient.Get(context.TODO(),
		types.NamespacedName{Name: sbrcName, Namespace: medik8sparams.OperatorNs}, sbrc); getErr != nil {
		return "", fmt.Errorf("get SBRC %s: %w", sbrcName, getErr)
	}

	mode, _, nestedErr := unstructured.NestedString(sbrc.Object,
		"spec", sbrparams.SBRCBlockVolumeModeField)
	if nestedErr != nil {
		return "", fmt.Errorf("read %s of SBRC %s: %w",
			sbrparams.SBRCBlockVolumeModeField, sbrcName, nestedErr)
	}

	return mode, nil
}
