package tests

import (
	"bytes"
	"context"
	"encoding/binary"
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
)

// SBD message layout, from internal/sbdprotocol/message.go (little-endian throughout).
const (
	// sbdMagic is SBD_MAGIC, the first 8 bytes of every SBD message.
	sbdMagic = "SBDMSG01"

	// sbdHeaderSize is SBD_HEADER_SIZE: 8 + 2 + 1 + 2 + 8 + 8 + 4.
	sbdHeaderSize = 33

	// sbdHeartbeatMsgType is SBD_MSG_TYPE_HEARTBEAT.
	sbdHeartbeatMsgType byte = 0x01
)

// SlotData represents a single heartbeat slot entry from the SBR device.
type SlotData struct {
	NodeID    uint16
	Timestamp int64
	Sequence  uint64
	HasData   bool
}

// readDeviceBlocksFromPod reads countBlocks blockSlotSize-sized blocks from the shared block
// device, starting at block skipBlocks, by running dd inside an SBR agent pod. iflag=direct
// bypasses the page cache so writes made by agents on other nodes are actually observed.
func readDeviceBlocksFromPod(podName, namespace string, skipBlocks, countBlocks int) ([]byte, error) {
	podBuilder, err := pod.Pull(APIClient, podName, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to pull pod %s: %w", podName, err)
	}

	cmd := []string{
		"dd", "if=" + sbrBlockDevicePath,
		"bs=" + strconv.Itoa(blockSlotSize),
		"skip=" + strconv.Itoa(skipBlocks),
		"count=" + strconv.Itoa(countBlocks),
		"iflag=direct", "status=none",
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
	if want := countBlocks * blockSlotSize; stdout.Len() != want {
		return nil, fmt.Errorf("short read of %s in pod %s: got %d bytes, want %d (stderr: %s)",
			sbrBlockDevicePath, podName, stdout.Len(), want, strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// readSlotDataFromPod reads the whole heartbeat region from an SBR agent pod and parses
// every written heartbeat slot out of it.
func readSlotDataFromPod(podName, namespace string) ([]SlotData, error) {
	data, err := readDeviceBlocksFromPod(podName, namespace, heartbeatRegionBlock, blockMaxNodes)
	if err != nil {
		return nil, err
	}

	return parseSlotData(data), nil
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
		slotBytes := data[slotIdx*blockSlotSize : slotIdx*blockSlotSize+sbdHeaderSize]

		// A slot only counts as written if it carries a valid heartbeat header.
		if !bytes.Equal(slotBytes[0:8], []byte(sbdMagic)) {
			continue
		}

		if slotBytes[10] != sbdHeartbeatMsgType {
			continue
		}

		want := crc32.ChecksumIEEE(slotBytes[0:29])
		if got := binary.LittleEndian.Uint32(slotBytes[29:33]); got != want {
			GinkgoWriter.Printf("  Slot %d: checksum mismatch (got %d, want %d), skipping\n", slotIdx, got, want)

			continue
		}

		slots = append(slots, SlotData{
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

// validateSequenceNumbers checks that sequence numbers are incrementing
// correctly and there are no conflicts between different agents.
func validateSequenceNumbers(slotDataByPod map[string][]SlotData) error {
	// Collect all slots with sequence numbers across all pods
	type sequenceEntry struct {
		PodName  string
		NodeID   uint16
		Sequence uint64
	}

	var allSequences []sequenceEntry

	for podName, slots := range slotDataByPod {
		for _, slot := range slots {
			if slot.HasData && slot.Sequence > 0 {
				allSequences = append(allSequences, sequenceEntry{
					PodName:  podName,
					NodeID:   slot.NodeID,
					Sequence: slot.Sequence,
				})
			}
		}
	}

	if len(allSequences) == 0 {
		GinkgoWriter.Printf("⚠ No sequence numbers found in slot data (agents may still be initializing)\n")

		return nil
	}

	GinkgoWriter.Printf("Found %d slot entries with sequence numbers\n", len(allSequences))

	// Group by NodeID and check sequence progression
	sequencesByNode := make(map[uint16][]uint64)

	for _, entry := range allSequences {
		sequencesByNode[entry.NodeID] = append(sequencesByNode[entry.NodeID], entry.Sequence)
	}

	GinkgoWriter.Printf("Detected %d unique node IDs writing heartbeats\n", len(sequencesByNode))

	// For each node, sequences should be incrementing
	for nodeID, sequences := range sequencesByNode {
		if len(sequences) == 0 {
			continue
		}

		// Check that we have incrementing sequences (allowing for some reads to miss recent writes)
		minSeq := sequences[0]
		maxSeq := sequences[0]

		for _, seq := range sequences {
			if seq < minSeq {
				minSeq = seq
			}
			if seq > maxSeq {
				maxSeq = seq
			}
		}

		GinkgoWriter.Printf("  Node ID %d: sequence range [%d - %d], samples: %d\n",
			nodeID, minSeq, maxSeq, len(sequences))

		// Sequences should be incrementing (max > min unless only 1 sample)
		if len(sequences) > 1 && maxSeq <= minSeq {
			return fmt.Errorf("sequences for node %d should be incrementing", nodeID)
		}
	}

	// Verify we see heartbeats from multiple nodes (proves concurrent writes)
	if len(sequencesByNode) < 2 {
		return fmt.Errorf("expected heartbeats from at least 2 different nodes, but found %d",
			len(sequencesByNode))
	}
	GinkgoWriter.Printf("✓ Sequence numbers are incrementing correctly\n")
	GinkgoWriter.Printf("✓ Detected concurrent writes from %d different nodes\n", len(sequencesByNode))

	return nil
}

// setupBlockModeSBRC creates an SBRC backed by Ceph RBD in Block volume mode and waits for its
// agent DaemonSet to come up. It fails the suite when SNR is running (both operators claim the
// watchdog) and skips it when the cluster has no Ceph RBD StorageClass. The returned object is
// the created SBRC, for the caller to delete in AfterAll.
func setupBlockModeSBRC(sbrcName string) *unstructured.Unstructured {
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

	By("Discovering Ceph RBD block storage class for Ceph RBD tests")

	// Skips the suite when the cluster has no Ceph RBD StorageClass.
	rbdStorageClass := discoverCephRBDStorageClass()

	By(fmt.Sprintf("Pre-cleaning stale SBRC %q if present", sbrcName))
	deleteSBRCAndWait(buildSBRC(sbrcName, map[string]interface{}{}), sbrcName)

	By(fmt.Sprintf("Creating StorageBasedRemediationConfig %q with Ceph RBD block storage", sbrcName))

	sbrc := buildSBRC(sbrcName, buildBlockModeSBRC(rbdStorageClass, nil))
	createErr := APIClient.Create(context.TODO(), sbrc)
	Expect(createErr).ToNot(HaveOccurred(),
		"StorageBasedRemediationConfig %q must be created for the Ceph RBD block storage test", sbrcName)

	By(fmt.Sprintf("Waiting for SBRC %q agent DaemonSet to be ready", sbrcName))
	waitForSBRCReady(sbrcName)

	return sbrc
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
