package tests

import (
	"context"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
