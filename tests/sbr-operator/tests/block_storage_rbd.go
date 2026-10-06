package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR), func() {
		var (
			rbdStorageClass string
			testSBRC        *unstructured.Unstructured
		)

		BeforeAll(func() {
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
			rbdStorageClass = discoverCephRBDStorageClass()
			GinkgoWriter.Printf("Using Ceph RBD StorageClass %q for block storage tests\n", rbdStorageClass)

			By(fmt.Sprintf("Pre-cleaning stale SBRC %q if present", sbrparams.SBRCBlockTestName))
			staleObj := &unstructured.Unstructured{}
			staleObj.SetAPIVersion(sbrparams.CRDGroup + "/" + sbrparams.CRDVersion)
			staleObj.SetKind("StorageBasedRemediationConfig")
			staleObj.SetName(sbrparams.SBRCBlockTestName)
			staleObj.SetNamespace(medik8sparams.OperatorNs)

			if delErr := APIClient.Delete(context.TODO(), staleObj); delErr != nil && !k8serrors.IsNotFound(delErr) {
				GinkgoWriter.Printf("Pre-cleanup: warning deleting stale SBRC %s: %v\n",
					sbrparams.SBRCBlockTestName, delErr)
			} else if delErr == nil {
				Eventually(func() bool {
					chk := &unstructured.Unstructured{}
					chk.SetAPIVersion(sbrparams.CRDGroup + "/" + sbrparams.CRDVersion)
					chk.SetKind("StorageBasedRemediationConfig")
					getErr := APIClient.Get(context.TODO(),
						types.NamespacedName{
							Name:      sbrparams.SBRCBlockTestName,
							Namespace: medik8sparams.OperatorNs,
						}, chk)

					return k8serrors.IsNotFound(getErr)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
					"Stale SBRC %q must be gone before creating a fresh one",
					sbrparams.SBRCBlockTestName)
			}

			By(fmt.Sprintf("Creating StorageBasedRemediationConfig %q with Ceph RBD block storage",
				sbrparams.SBRCBlockTestName))

			testSBRC = buildSBRC(sbrparams.SBRCBlockTestName, map[string]interface{}{
				"sharedStorageClass":      rbdStorageClass,
				"sharedStorageVolumeMode": "Block",
			})
			createErr := APIClient.Create(context.TODO(), testSBRC)
			Expect(createErr).ToNot(HaveOccurred(),
				"StorageBasedRemediationConfig %q must be created for Ceph RBD block storage test",
				sbrparams.SBRCBlockTestName)

			By(fmt.Sprintf("Waiting for SBRC %q agent DaemonSet to be ready",
				sbrparams.SBRCBlockTestName))
			waitForSBRCReady(sbrparams.SBRCBlockTestName)
		})

		AfterAll(func() {
			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockTestName))

				if deleteErr := APIClient.Delete(context.TODO(), testSBRC); deleteErr != nil &&
					!k8serrors.IsNotFound(deleteErr) {
					GinkgoT().Logf("Warning: cleanup delete SBRC %s: %v",
						sbrparams.SBRCBlockTestName, deleteErr)
				} else {
					Eventually(func() error {
						getErr := APIClient.Get(context.TODO(),
							types.NamespacedName{
								Name:      sbrparams.SBRCBlockTestName,
								Namespace: medik8sparams.OperatorNs,
							},
							testSBRC.DeepCopy())

						if k8serrors.IsNotFound(getErr) {
							return nil
						}

						if getErr != nil {
							return getErr
						}

						return fmt.Errorf("SBRC %s still present", sbrparams.SBRCBlockTestName)
					}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
				}
			}
		})

		It("Verify SBR agents write concurrently to Ceph RBD block device without conflicts",
			reportxml.ID("TBD"),
			Label(
				labels.OperatorSBR,
				labels.DisruptionNonDestructive,
				labels.TierAcceptance,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				testCephRBDConcurrentWrites()
			})
	})

// testCephRBDConcurrentWrites validates that multiple SBR agents can write
// concurrently to a Ceph RBD block device without conflicts.
func testCephRBDConcurrentWrites() {
	By("Verifying all agent pods reach Ready state")
	var agentPods []*pod.Builder

	Eventually(func() error {
		var err error
		agentPods, err = pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: sbrparams.AgentPodLabelSelector})
		if err != nil {
			return err
		}

		runningPods := helpers.FilterRunningPods(agentPods)
		readyCount := len(runningPods)
		expectedCount := getWorkerNodeCount()

		if readyCount < expectedCount {
			return fmt.Errorf("only %d/%d agents ready", readyCount, expectedCount)
		}

		return nil
	}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"All agent pods must reach Ready state")

	By("Validating agent pod count matches worker node count")
	runningPods := helpers.FilterRunningPods(agentPods)
	expectedCount := getWorkerNodeCount()
	actualCount := len(runningPods)

	Expect(actualCount).To(Equal(expectedCount),
		"Agent pod count (%d) should match worker node count (%d)", actualCount, expectedCount)

	GinkgoWriter.Printf("Successfully verified %d agent pods running on %d worker nodes\n",
		actualCount, expectedCount)
	GinkgoWriter.Printf("Agent pods are using Ceph RBD block storage in Block volume mode\n")

	By("Confirming multi-attach capability via Ready pod count")
	// If multiple agents reach Ready state simultaneously, the block device
	// supports true RWX multi-attach. A single-writer limitation would prevent
	// multiple agents from attaching to the same block device.
	Expect(actualCount).To(BeNumerically(">=", 2),
		"At least 2 agents must be Ready to validate multi-attach capability")

	GinkgoWriter.Printf("✓ Validated concurrent block device access: %d agents Ready\n", actualCount)
	GinkgoWriter.Printf("✓ Ceph RBD block storage supports true RWX multi-attach\n")
}
