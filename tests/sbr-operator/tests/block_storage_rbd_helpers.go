package tests

import (
	"context"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// discoverCephRBDStorageClass returns a Ceph RBD block storage class name.
// Checks the SBR_RBD_STORAGE_CLASS env var first; falls back to auto-discovery by provisioner.
// Skips the test when no Ceph RBD class is found because the test requires RBD-specific features.
func discoverCephRBDStorageClass() string {
	if envClass := os.Getenv("SBR_RBD_STORAGE_CLASS"); envClass != "" {
		obj, getErr := APIClient.StorageV1Interface.StorageClasses().Get(
			context.TODO(), envClass, metav1.GetOptions{})
		Expect(getErr).ToNot(HaveOccurred(), "SBR_RBD_STORAGE_CLASS=%q not found", envClass)

		if !strings.Contains(obj.Provisioner, "rbd.csi.ceph.com") {
			GinkgoWriter.Printf("WARNING: SBR_RBD_STORAGE_CLASS=%q uses provisioner %q (not Ceph RBD); "+
				"test may not work correctly\n", envClass, obj.Provisioner)
		}

		GinkgoWriter.Printf("Using SBR_RBD_STORAGE_CLASS=%q (provisioner: %s)\n", envClass, obj.Provisioner)

		return envClass
	}

	// Auto-discover Ceph RBD StorageClass
	scList, err := APIClient.StorageV1Interface.StorageClasses().List(context.TODO(), metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list StorageClasses")

	for idx := range scList.Items {
		provisioner := scList.Items[idx].Provisioner
		// ODF Ceph RBD provisioner pattern
		if strings.Contains(provisioner, "rbd.csi.ceph.com") {
			GinkgoWriter.Printf("Auto-discovered Ceph RBD StorageClass: %s (provisioner: %s)\n",
				scList.Items[idx].Name, provisioner)

			return scList.Items[idx].Name
		}
	}

	Skip("No Ceph RBD StorageClass found; install ODF or set SBR_RBD_STORAGE_CLASS env var")

	return ""
}

// checkSBRStorageUnhealthyCondition returns true if the named node has SBRStorageUnhealthy=True.
func checkSBRStorageUnhealthyCondition(nodeName string) bool {
	node, err := APIClient.CoreV1Interface.Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("Warning: failed to get node %s: %v\n", nodeName, err)
		return false
	}

	for _, condition := range node.Status.Conditions {
		if string(condition.Type) == "SBRStorageUnhealthy" && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

// getAllWorkerNodes returns all worker nodes (not control-plane).
func getAllWorkerNodes() []corev1.Node {
	nodeList, err := APIClient.CoreV1Interface.Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/worker",
	})
	Expect(err).ToNot(HaveOccurred(), "Failed to list worker nodes")

	return nodeList.Items
}

// getWorkerNodeCount returns the count of worker nodes.
func getWorkerNodeCount() int {
	return len(getAllWorkerNodes())
}

// buildPerfTestSBRC creates an SBRC for the performance test with Ceph RBD block storage.
func buildPerfTestSBRC(rbdStorageClass, sbrcName string) map[string]interface{} {
	return map[string]interface{}{
		"sharedStorageClass":      rbdStorageClass,
		"sharedStorageVolumeMode": "Block",
		// Use minimum timeout for maximum stress
		"sbrTimeoutSeconds": int64(10),
		// Standard failure threshold
		"maxConsecutiveFailures": int64(3),
	}
}
