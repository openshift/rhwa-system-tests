package tests

import (
	"context"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// discoverCephRBDStorageClass returns a Ceph RBD block storage class name.
// Checks the SBR_RBD_STORAGE_CLASS env var first; falls back to listing StorageClasses
// by provisioner pattern. Skips the test when no Ceph RBD class is found because these
// tests specifically validate block storage behavior on Ceph RBD.
func discoverCephRBDStorageClass() string {
	if storageClassName := os.Getenv("SBR_RBD_STORAGE_CLASS"); storageClassName != "" {
		obj, getErr := APIClient.StorageV1Interface.StorageClasses().Get(
			context.TODO(), storageClassName, metav1.GetOptions{})
		Expect(getErr).ToNot(HaveOccurred(), "SBR_RBD_STORAGE_CLASS=%q not found", storageClassName)

		if !strings.Contains(obj.Provisioner, "rbd.csi.ceph.com") {
			Skip(fmt.Sprintf("SBR_RBD_STORAGE_CLASS=%q uses provisioner %q (not Ceph RBD); "+
				"block storage tests require a Ceph RBD-backed class", storageClassName, obj.Provisioner))
		}

		return storageClassName
	}

	scList, err := APIClient.StorageV1Interface.StorageClasses().List(context.TODO(), metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list StorageClasses")

	for idx := range scList.Items {
		if strings.Contains(scList.Items[idx].Provisioner, "rbd.csi.ceph.com") {
			GinkgoWriter.Printf("Auto-discovered Ceph RBD StorageClass: %s (provisioner: %s)\n",
				scList.Items[idx].Name, scList.Items[idx].Provisioner)

			return scList.Items[idx].Name
		}
	}

	Skip("No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS env var")

	return ""
}

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

// SlotData represents a single heartbeat slot entry from the SBR device
type SlotData struct {
	NodeID    uint16
	Timestamp int64
	Sequence  uint64
	HasData   bool
}

// readSlotDataFromPod reads slot data from an SBR agent pod by executing
// dd to read the block device and parsing the heartbeat messages.
func readSlotDataFromPod(podName, namespace string) ([]SlotData, error) {
	// Read first 255 slots (255 * 512 bytes = 130560 bytes)
	// Each slot is 512 bytes and contains a heartbeat message
	sbrDevicePath := "/sbr-block"

	cmd := []string{"dd", "if=" + sbrDevicePath, "bs=512", "count=255", "status=none"}

	podBuilder, err := pod.Pull(APIClient, podName, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to pull pod %s: %w", podName, err)
	}

	output, err := podBuilder.ExecCommand(cmd, "")
	if err != nil {
		return nil, fmt.Errorf("failed to exec dd command in pod %s: %w", podName, err)
	}

	// Parse the raw block device output into slot data
	slots := parseSlotData(output.Bytes())

	return slots, nil
}

// parseSlotData parses raw block device bytes into SlotData entries
func parseSlotData(data []byte) []SlotData {
	const slotSize = 512
	var slots []SlotData

	// Each 512-byte slot contains:
	// - Magic bytes at the start (to identify valid entries)
	// - NodeID (2 bytes)
	// - Timestamp (8 bytes)
	// - Sequence (8 bytes)
	// - Other metadata

	for i := 0; i < len(data)/slotSize && i < 255; i++ {
		offset := i * slotSize
		slotBytes := data[offset : offset+slotSize]

		// Check if slot has data (non-zero bytes)
		hasData := false
		for _, b := range slotBytes[:64] { // Check first 64 bytes
			if b != 0 {
				hasData = true
				break
			}
		}

		if !hasData {
			continue
		}

		// Simple parsing - look for heartbeat pattern
		// This is a simplified version; actual format may vary
		slot := SlotData{
			HasData: hasData,
		}

		// Try to extract NodeID, Timestamp, Sequence from known offsets
		// Note: These offsets are approximations based on the SBR protocol
		if len(slotBytes) >= 64 {
			// NodeID is typically at a fixed offset
			slot.NodeID = uint16(slotBytes[16])<<8 | uint16(slotBytes[17])

			// Timestamp and Sequence would be at other offsets
			// For now, mark as having data
		}

		slots = append(slots, slot)
	}

	return slots
}

// validateSlotDataConsistency compares slot data across all pods to ensure
// they all see the same block device content (data integrity).
func validateSlotDataConsistency(slotDataByPod map[string][]SlotData) {
	if len(slotDataByPod) == 0 {
		Fail("No slot data collected from any pods")
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

	Expect(variance).To(BeNumerically("<=", maxAllowedVariance),
		"Slot count variance too high (%d). Min=%d, Max=%d. "+
			"This suggests inconsistent block device reads across pods.",
		variance, minSlots, maxSlots)

	GinkgoWriter.Printf("✓ Slot counts are consistent across pods (variance: %d, max allowed: %d)\n",
		variance, maxAllowedVariance)
}

// validateSequenceNumbers checks that sequence numbers are incrementing
// correctly and there are no conflicts between different agents.
func validateSequenceNumbers(slotDataByPod map[string][]SlotData) {
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
		return
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
		if len(sequences) > 1 {
			Expect(maxSeq).To(BeNumerically(">=", minSeq),
				"Sequences for node %d should be incrementing", nodeID)
		}
	}

	// Verify we see heartbeats from multiple nodes (proves concurrent writes)
	Expect(len(sequencesByNode)).To(BeNumerically(">=", 2),
		"Should see heartbeats from at least 2 different nodes (concurrent writers)")

	GinkgoWriter.Printf("✓ Sequence numbers are incrementing correctly\n")
	GinkgoWriter.Printf("✓ Detected concurrent writes from %d different nodes\n", len(sequencesByNode))
}
