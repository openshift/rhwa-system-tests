package tests

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
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
