package tests

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// discoverRWXStorageClass is defined in sbr.go.
// pickTargetWorkerNode and getSBRCRCondition are defined in sbr_helpers.go.

// getNodeConditionNHC fetches a named condition from a node's status.
// Returns nil when the condition is not present or the node cannot be retrieved.
func getNodeConditionNHC(ctx context.Context, nodeName, condType string) *corev1.NodeCondition {
	node, err := APIClient.CoreV1Interface.Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil
	}

	for i := range node.Status.Conditions {
		if string(node.Status.Conditions[i].Type) == condType {
			return &node.Status.Conditions[i]
		}
	}

	return nil
}

func buildNHCUnstructured() *unstructured.Unstructured {
	return buildNHC(sbrparams.NHCTestName)
}

var _ = Describe(
	"SBR Functional — NHC Integration",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR), func() {
		var (
			targetNodeName  string
			testSBRC        *unstructured.Unstructured
			nhcCR           *unstructured.Unstructured
			nhcCreatedByUs  bool
			storageClass    string
			injectorPodName string
			preRebootBootID string
		)

		BeforeAll(func() {
			By("Checking whether NHC CRD is installed")

			crd := &apiextensionsv1.CustomResourceDefinition{}
			crdErr := APIClient.Get(context.TODO(),
				types.NamespacedName{Name: sbrparams.NHCCRDName}, crd)

			if k8serrors.IsNotFound(crdErr) {
				Skip("NodeHealthCheck CRD not found — NHC operator not installed; skipping NHC integration test")
			}

			Expect(crdErr).ToNot(HaveOccurred(),
				"Unexpected error while checking for NodeHealthCheck CRD")

			By("Ensuring StorageBasedRemediationTemplate exists")

			ensureSBRTemplate()

			By("Discovering RWX storage class")

			storageClass = discoverRWXStorageClass()

			By("Pre-cleanup: removing any stale SBRC from prior runs")

			staleRef := buildSBRC(sbrparams.SBRCNHCTestName, map[string]interface{}{})
			if deleteErr := APIClient.Delete(context.TODO(), staleRef); deleteErr != nil &&
				!k8serrors.IsNotFound(deleteErr) {
				GinkgoT().Logf("Warning: pre-cleanup delete %s: %v", sbrparams.SBRCNHCTestName, deleteErr)
			}

			Eventually(func() error {
				getErr := APIClient.Get(context.TODO(),
					types.NamespacedName{Name: sbrparams.SBRCNHCTestName, Namespace: medik8sparams.OperatorNs},
					buildSBRC(sbrparams.SBRCNHCTestName, map[string]interface{}{}))
				if k8serrors.IsNotFound(getErr) {
					return nil
				}

				if getErr != nil {
					return getErr
				}

				return fmt.Errorf("SBRC %s still terminating", sbrparams.SBRCNHCTestName)
			}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed())

			By("Clearing residual Ceph OSD blocklist entries from prior tests")
			clearCephOSDBlocklist()

			By(fmt.Sprintf("Creating StorageBasedRemediationConfig %q with sharedStorageClass=%q",
				sbrparams.SBRCNHCTestName, storageClass))

			// The minimum 10s timeout leaves only about 15s between NHC starting
			// remediation and SBR making the condition Unknown. A 30s hardware
			// watchdog needs a longer remediation window to complete a reset.
			testSBRC = buildSBRC(sbrparams.SBRCNHCTestName, map[string]interface{}{
				"sharedStorageClass": storageClass,
				"sbrTimeoutSeconds":  int64(sbrparams.NHCIntegrationSBRTimeoutSeconds),
			})
			createErr := APIClient.Create(context.TODO(), testSBRC)
			Expect(createErr).ToNot(HaveOccurred(),
				"Failed to create StorageBasedRemediationConfig %q", sbrparams.SBRCNHCTestName)

			By("Waiting for agent DaemonSet to have at least one ready pod")

			waitForSBRCReady(sbrparams.SBRCNHCTestName)

			By("Creating NodeHealthCheck CR if it does not already exist")

			existingNHC := &unstructured.Unstructured{}
			existingNHC.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   sbrparams.NHCAPIGroup,
				Version: sbrparams.NHCAPIVersion,
				Kind:    "NodeHealthCheck",
			})
			getErr := APIClient.Get(context.TODO(),
				types.NamespacedName{Name: sbrparams.NHCTestName}, existingNHC)

			switch {
			case k8serrors.IsNotFound(getErr):
				nhcCR = buildNHCUnstructured()
				nhcCreateErr := APIClient.Create(context.TODO(), nhcCR)
				Expect(nhcCreateErr).ToNot(HaveOccurred(),
					"Failed to create NodeHealthCheck CR %q", sbrparams.NHCTestName)
				nhcCreatedByUs = true

			case getErr != nil:
				Expect(getErr).ToNot(HaveOccurred(),
					"Unexpected error fetching NodeHealthCheck %q", sbrparams.NHCTestName)

			default:
				// Validate the existing CR has the spec this test requires.
				// A mismatched remediationTemplate or unhealthyConditions would cause the 5-minute
				// NHC→SBR CR wait to time out with no actionable error.
				expectedSpec := buildNHCUnstructured().Object["spec"]
				existingSpec, specFound, specErr := unstructured.NestedFieldNoCopy(
					existingNHC.Object, "spec")
				if specErr != nil || !specFound {
					Skip(fmt.Sprintf(
						"NodeHealthCheck %q exists but has no readable spec; "+
							"delete it and re-run to let the test create its own",
						sbrparams.NHCTestName))
				}

				if fmt.Sprintf("%v", existingSpec) != fmt.Sprintf("%v", expectedSpec) {
					Skip(fmt.Sprintf(
						"NodeHealthCheck %q exists with a different spec than the test requires — "+
							"delete it and re-run to let the test create its own.\n"+
							"  existing: %v\n  expected: %v",
						sbrparams.NHCTestName, existingSpec, expectedSpec))
				}

				nhcCR = existingNHC
				nhcCreatedByUs = false
				GinkgoWriter.Printf("NodeHealthCheck %q already exists with matching spec; reusing it\n",
					sbrparams.NHCTestName)
			}

			By("Selecting a target worker node (schedulable, not hosting SBR controller pods)")

			targetNodeName = pickTargetWorkerNode()

			if targetNodeName == "" {
				Skip("No schedulable worker node available that does not host an SBR controller pod; " +
					"skipping NHC integration test")
			}

			injectorPodName = sbrparams.NHCInjectorPodName + "-" +
				strings.Map(func(r rune) rune {
					if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
						return r
					}

					return '-'
				}, strings.ToLower(targetNodeName))

			if len(injectorPodName) > 253 {
				injectorPodName = injectorPodName[:253]
			}

			injectorPodName = strings.TrimRight(injectorPodName, "-")
			GinkgoWriter.Printf("Target node: %q | injector pod: %q\n", targetNodeName, injectorPodName)
		})

		AfterAll(func() {
			By("AfterAll: removing injected CephFS REJECT rules before deleting the NHC")
			removeInjectedRejectRules(injectorPodName, removeCephFSRejectOutput)

			By("Clearing residual Ceph OSD blocklist entries left by the fencing")
			clearCephOSDBlocklist()

			By("AfterAll: removing NodeHealthCheck CR if created by this test")

			if nhcCreatedByUs && nhcCR != nil {
				// cleanupNHCCR retries the delete until the CR is fully gone: the NHC
				// webhook can reject a single delete while a remediation is still
				// terminating after the fault removal above, and a half-terminated CR
				// would be reused as-is by the next run's BeforeAll.
				cleanupNHCCR(context.Background(), sbrparams.NHCTestName)
			}

			By("AfterAll: force-removing any leftover StorageBasedRemediation CR")

			if targetNodeName != "" {
				cleanupSBRCR(context.Background(), targetNodeName)
			}

			By("AfterAll: deleting StorageBasedRemediationConfig")

			if testSBRC != nil {
				deleteErr := APIClient.Delete(context.TODO(), testSBRC)
				if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
					GinkgoT().Logf("Warning: cleanup delete StorageBasedRemediationConfig %s: %v",
						sbrparams.SBRCNHCTestName, deleteErr)
				}
			}

			By("AfterAll: removing injector pod if still present")

			if injectorPodName != "" {
				if injectorPod, pullErr := pod.Pull(APIClient, injectorPodName, medik8sparams.OperatorNs); pullErr == nil {
					if _, delErr := injectorPod.Delete(); delErr != nil && !k8serrors.IsNotFound(delErr) {
						GinkgoT().Logf("Warning: cleanup delete injector pod %s: %v", injectorPodName, delErr)
					}
				}
			}

			if targetNodeName != "" && preRebootBootID != "" {
				bootID, err := getNodeBootID(targetNodeName)
				if err != nil || bootID == preRebootBootID {
					GinkgoWriter.Printf("Skipping injector image purge: no confirmed reboot of %s\n", targetNodeName)

					return
				}
				By("AfterAll: removing the injector image from the rebooted target node")
				purgeInjectorImageAfterReboot(context.Background(), targetNodeName)
			}
		})

		It("End-to-end: NHC detects storage failure and triggers SBR fencing",
			reportxml.ID("88879"),
			Label(
				labels.OperatorSBR,
				labels.TierAcceptance,
				labels.FrequencyNightly,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.ComponentRemediation,
			), func() {
				DeferCleanup(func() {
					By("DeferCleanup: removing iptables REJECT rules on target node")

					cleanupPod, pullErr := pod.Pull(APIClient, injectorPodName, medik8sparams.OperatorNs)
					if pullErr == nil {
						removeCephFSRejectOutput(cleanupPod)

						if _, delErr := cleanupPod.Delete(); delErr != nil {
							GinkgoWriter.Printf("Warning: delete injector pod: %v\n", delErr)
						}
					}

					By("DeferCleanup: force-removing StorageBasedRemediation CR if still present")

					cleanupSBRCR(context.Background(), targetNodeName)
				})

				By("Recording pre-injection BootID (baseline before watchdog reboot)")

				var preBootIDErr error

				preRebootBootID, preBootIDErr = getNodeBootID(targetNodeName)
				Expect(preBootIDErr).ToNot(HaveOccurred(),
					"Failed to record pre-injection BootID on node %q", targetNodeName)
				GinkgoWriter.Printf("Node %q pre-injection BootID: %s\n", targetNodeName, preRebootBootID)

				By(fmt.Sprintf("Creating privileged injector pod on node %q", targetNodeName))

				injectorPod, createErr := pod.NewBuilder(
					APIClient, injectorPodName, medik8sparams.OperatorNs, sbrparams.InjectorImage).
					DefineOnNode(targetNodeName).
					WithHostPid(true).
					WithPrivilegedFlag().
					CreateAndWaitUntilRunning(medik8sparams.DefaultTimeout)
				Expect(createErr).ToNot(HaveOccurred(),
					"Failed to create injector pod on node %q", targetNodeName)
				injectCephFSRejectOutput(injectorPod, targetNodeName)

				By(fmt.Sprintf("Waiting for node %q to acquire SBRStorageUnhealthy=True condition", targetNodeName))

				Eventually(func() error {
					cond := getNodeConditionNHC(
						context.TODO(), targetNodeName, sbrparams.SBRStorageUnhealthyCondition)
					if cond == nil {
						return fmt.Errorf("node %s: condition %s not yet present",
							targetNodeName, sbrparams.SBRStorageUnhealthyCondition)
					}

					if cond.Status != corev1.ConditionTrue {
						return fmt.Errorf("node %s: %s=%s, want True",
							targetNodeName, sbrparams.SBRStorageUnhealthyCondition, cond.Status)
					}

					return nil
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"Node %q must reach %s=True after storage injection",
					targetNodeName, sbrparams.SBRStorageUnhealthyCondition)

				By(fmt.Sprintf("Waiting for NHC to create StorageBasedRemediation CR for node %q", targetNodeName))

				Eventually(func() error {
					_, getErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(getErr) {
						return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", targetNodeName)
					}

					return getErr
				}, sbrparams.NHCSBRCRCreationTimeout, sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create StorageBasedRemediation CR for node %q within timeout", targetNodeName)

				GinkgoWriter.Printf("StorageBasedRemediation CR for node %q created by NHC\n", targetNodeName)

				By("Waiting for the controller to start fencing (FencingInProgress or FencingSucceeded)")

				// FencingInProgress=True can last less than one poll interval: the
				// storage heartbeat already stopped because of the injected REJECT
				// rules, so the controller proves the fence on its next reconcile
				// and moves straight to FencingSucceeded. After the node reboots and
				// recovers, NHC deletes the CR. Any of these states shows that
				// fencing started; the BootID check below proves the reboot.
				Eventually(func() error {
					sbrObj, getErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(getErr) {
						GinkgoWriter.Printf("StorageBasedRemediation/%s already removed after fencing\n", targetNodeName)

						return nil
					}

					if getErr != nil {
						return getErr
					}

					for _, condType := range []string{
						sbrparams.FencingInProgressCondition, sbrparams.FencingSucceededCondition,
					} {
						if cond := getSBRCRCondition(sbrObj, condType); cond != nil &&
							cond["status"] == string(corev1.ConditionTrue) {
							GinkgoWriter.Printf("StorageBasedRemediation/%s: %s=True\n", targetNodeName, condType)

							return nil
						}
					}

					return fmt.Errorf("StorageBasedRemediation/%s: neither %s nor %s is True",
						targetNodeName, sbrparams.FencingInProgressCondition, sbrparams.FencingSucceededCondition)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must start fencing", targetNodeName)

				By(fmt.Sprintf("Waiting for node %q to go NotReady then return Ready (reboot cycle)", targetNodeName))

				// Phase 1: wait for NotReady.
				Eventually(func() error {
					return nodeRebootStarted(targetNodeName, preRebootBootID)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must become NotReady during reboot", targetNodeName)

				GinkgoWriter.Printf("Node %q is NotReady or already rebooted\n", targetNodeName)

				// Phase 2: wait for Ready.
				Eventually(func() error {
					node, nodeGetErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), targetNodeName, metav1.GetOptions{})
					if nodeGetErr != nil {
						return nodeGetErr
					}

					for _, cond := range node.Status.Conditions {
						if cond.Type == corev1.NodeReady {
							if cond.Status == corev1.ConditionTrue {
								return nil
							}

							return fmt.Errorf("node %s condition Ready=%s", targetNodeName, cond.Status)
						}
					}

					return fmt.Errorf("node %s has no Ready condition", targetNodeName)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must return to Ready after reboot", targetNodeName)

				GinkgoWriter.Printf("Node %q is Ready again\n", targetNodeName)

				By("Verifying BootID changed (confirms actual reboot, not just transient state)")

				newBootID, newBootIDErr := getNodeBootID(targetNodeName)
				Expect(newBootIDErr).ToNot(HaveOccurred(),
					"Failed to read post-reboot BootID on node %q", targetNodeName)
				Expect(newBootID).ToNot(Equal(preRebootBootID),
					"Node %q BootID %q matches pre-reboot BootID %q — "+
						"node did not actually reboot",
					targetNodeName, newBootID, preRebootBootID)

				By("Waiting for FencingSucceeded=True on the StorageBasedRemediation CR")

				// Design note: IsNotFound is treated as success because the controller's normal
				// cleanup flow sets FencingSucceeded=True then immediately deletes the CR, so a
				// poll that lands after deletion has no condition left to observe. Whether any
				// poll sees the condition depends on poll timing against that short window.
				// The BootID assertion above is the proof that the fence rebooted the node;
				// this wait verifies the CR lifecycle finished, not that the condition was seen.
				Eventually(func() error {
					sbrObj, getErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(getErr) {
						// CR already cleaned up by controller — success.
						return nil
					}

					if getErr != nil {
						return getErr
					}

					cond := getSBRCRCondition(sbrObj, sbrparams.FencingSucceededCondition)
					if cond == nil {
						return fmt.Errorf("StorageBasedRemediation/%s: %s condition not present",
							targetNodeName, sbrparams.FencingSucceededCondition)
					}

					if cond["status"] != string(corev1.ConditionTrue) {
						return fmt.Errorf("StorageBasedRemediation/%s: %s=%v, want True",
							targetNodeName, sbrparams.FencingSucceededCondition, cond["status"])
					}

					return nil
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must reach FencingSucceeded=True or be cleaned up after fencing",
					targetNodeName)

				By("Waiting for the SBR CR to be deleted (controller cleanup after successful fencing)")

				Eventually(func() error {
					_, getErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(getErr) {
						return nil
					}

					if getErr != nil {
						return getErr
					}

					return fmt.Errorf("StorageBasedRemediation/%s still exists; waiting for controller cleanup",
						targetNodeName)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must be deleted by controller after successful fencing", targetNodeName)

				GinkgoWriter.Printf("NHC → SBR end-to-end fencing completed successfully for node %q\n", targetNodeName)
			})
	})
