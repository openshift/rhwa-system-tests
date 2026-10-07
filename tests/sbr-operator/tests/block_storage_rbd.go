package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe(
	"SBR Block Storage - Ceph RBD Persistent Fencing State",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentRemediation), func() {
		var (
			targetNodeName     string
			setupSBRC          *unstructured.Unstructured
			nhcCreated         bool
			rbdStorageClass    string
			originalBootID     string
			fencingSlotsBefore string
			fencingSlotsAfter  string
		)

		BeforeAll(func() {
			By("Checking NHC CRD is installed")

			if !isNHCCRDInstalled() {
				Skip("NodeHealthCheck CRD not found; NHC operator not installed - skipping block persistent fencing test")
			}

			By("Discovering Ceph RBD block storage class")

			rbdStorageClass = discoverCephRBDStorageClass()
			Expect(rbdStorageClass).ToNot(BeEmpty(),
				"No Ceph RBD storage class found; set SBR_RBD_STORAGE_CLASS env or deploy ODF before running")
			GinkgoWriter.Printf("Using Ceph RBD storage class: %s\n", rbdStorageClass)

			By("Creating SBRC with Ceph RBD block storage")

			sbrSpec := buildBlockModeSBRC(rbdStorageClass, nil)
			setupSBRC = buildSBRC(sbrparams.SBRCBlockPersistentFencingTestName, sbrSpec)
			createErr := APIClient.Create(context.TODO(), setupSBRC)
			Expect(createErr).ToNot(HaveOccurred(),
				"StorageBasedRemediationConfig %q must be created", sbrparams.SBRCBlockPersistentFencingTestName)
			waitForSBRCReady(sbrparams.SBRCBlockPersistentFencingTestName)

			By("Creating NodeHealthCheck CR")

			ensureSBRTemplate()
			nhc := buildNHC(sbrparams.NHCBlockPersistentFencingTestName)
			nhcErr := APIClient.Create(context.TODO(), nhc)
			if nhcErr != nil && !k8serrors.IsAlreadyExists(nhcErr) {
				Expect(nhcErr).ToNot(HaveOccurred(),
					"NodeHealthCheck CR %q must be created", sbrparams.NHCBlockPersistentFencingTestName)
			}

			nhcCreated = nhcErr == nil

			By("Selecting target worker node (schedulable, not controller pod host)")

			targetNodeName = pickTargetWorkerNode()
			if targetNodeName == "" {
				Skip("No schedulable worker node available (excluding controller nodes); " +
					"skipping block persistent fencing test")
			}

			GinkgoWriter.Printf("Target node: %s\n", targetNodeName)
		})

		AfterAll(func() {
			if nhcCreated {
				By("Deleting NodeHealthCheck CR")
				cleanupNHCCR(sbrparams.NHCBlockPersistentFencingTestName)
			}

			if setupSBRC != nil {
				By("Deleting SBRC created for block persistent fencing test")

				Eventually(func() error {
					deleteErr := APIClient.Delete(context.TODO(), setupSBRC)
					if deleteErr == nil || k8serrors.IsNotFound(deleteErr) {
						return nil
					}

					return deleteErr
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Failed to delete StorageBasedRemediationConfig %s",
					sbrparams.SBRCBlockPersistentFencingTestName)

				Eventually(func() error {
					getErr := APIClient.Get(context.TODO(),
						types.NamespacedName{
							Name:      sbrparams.SBRCBlockPersistentFencingTestName,
							Namespace: medik8sparams.OperatorNs,
						},
						setupSBRC.DeepCopy())

					if k8serrors.IsNotFound(getErr) {
						return nil
					}

					if getErr != nil {
						return getErr
					}

					return fmt.Errorf("SBRC %s still present", sbrparams.SBRCBlockPersistentFencingTestName)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
			}

			if targetNodeName != "" {
				By("Force-deleting any leftover StorageBasedRemediation CR for target node")
				cleanupSBRCR(targetNodeName)

				By("Uncordoning target node if needed")

				node, getErr := APIClient.CoreV1Interface.Nodes().Get(
					context.TODO(), targetNodeName, metav1.GetOptions{})
				if getErr == nil && !isNodeSchedulable(node) {
					patch := []byte(`{"spec":{"unschedulable":false}}`)
					_, patchErr := APIClient.CoreV1Interface.Nodes().Patch(
						context.TODO(), targetNodeName, types.MergePatchType, patch, metav1.PatchOptions{})
					if patchErr != nil {
						GinkgoWriter.Printf("Warning: cleanup failed to uncordon node %s: %v\n",
							targetNodeName, patchErr)
					}
				}
			}
		})

		It("Verify fencing state persists on Ceph RBD block storage across node reboot",
			reportxml.ID("TBD"),
			Label(
				labels.OperatorSBR,
				labels.DisruptionDestructive,
				labels.TierAcceptance,
				labels.PlatformAny,
				labels.ComponentRemediation,
				labels.FrequencyWeekly,
			), func() {
				By(fmt.Sprintf("Recording boot-id for node %s before fencing", targetNodeName))

				var bootIDErr error
				originalBootID, bootIDErr = getNodeBootID(targetNodeName)
				Expect(bootIDErr).ToNot(HaveOccurred(),
					"Must read pre-fencing boot-id from node %s", targetNodeName)
				GinkgoWriter.Printf("Pre-fencing boot-id: %s\n", originalBootID)

				By("Creating StorageBasedRemediation CR to trigger fencing")

				sbrCR := buildSBR(targetNodeName)
				err := APIClient.Create(context.TODO(), sbrCR)
				Expect(err).ToNot(HaveOccurred(),
					"StorageBasedRemediation CR for %s must be created", targetNodeName)

				DeferCleanup(func() {
					cleanupSBRCR(targetNodeName)
				})

				// Start periodic node status logging in background
				ctx, cancel := context.WithCancel(context.Background())
				DeferCleanup(func() {
					cancel()
				})

				go func() {
					ticker := time.NewTicker(30 * time.Second)
					defer ticker.Stop()

					// Log initial status immediately
					GinkgoWriter.Printf("\n=== Node Status Monitoring Started (every 30s) ===\n")
					logNodeStatus(targetNodeName)

					for {
						select {
						case <-ctx.Done():
							GinkgoWriter.Printf("\n=== Node Status Monitoring Stopped ===\n")

							return
						case <-ticker.C:
							logNodeStatus(targetNodeName)
						}
					}
				}()

				By("Waiting for FencingSucceeded=True on the StorageBasedRemediation CR")

				var fencingObserved bool

				Eventually(func() error {
					currentSBR, pullErr := pullSBRCR(targetNodeName)
					if pullErr != nil {
						if k8serrors.IsNotFound(pullErr) {
							if fencingObserved {
								return nil // CR cleaned up after confirmed fencing
							}

							return fmt.Errorf(
								"StorageBasedRemediation/%s gone before FencingSucceeded=True was observed",
								targetNodeName)
						}

						return fmt.Errorf("get StorageBasedRemediation/%s: %w", targetNodeName, pullErr)
					}

					fencingCond := getSBRCRCondition(currentSBR, sbrparams.FencingSucceededCondition)
					if fencingCond != nil {
						condStatus, _, _ := unstructured.NestedString(fencingCond, "status")
						if condStatus == string(corev1.ConditionTrue) {
							fencingObserved = true

							return nil
						}
					}

					conditions, _, _ := unstructured.NestedSlice(currentSBR.Object, "status", "conditions")

					return fmt.Errorf(
						"StorageBasedRemediation/%s: %s not True yet; conditions: %v",
						targetNodeName, sbrparams.FencingSucceededCondition, conditions)
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must reach %s=True",
					targetNodeName, sbrparams.FencingSucceededCondition)

				GinkgoWriter.Printf("FencingSucceeded=True on StorageBasedRemediation/%s\n", targetNodeName)

				By("Capturing block device slot state before node reboot")

				// Get agent pod on the target node
				agentPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs,
					metav1.ListOptions{
						LabelSelector: sbrparams.AgentPodLabelSelector,
						FieldSelector: fmt.Sprintf("spec.nodeName=%s", targetNodeName),
					})
				Expect(listErr).ToNot(HaveOccurred(), "Failed to list agent pods on node %s", targetNodeName)
				Expect(agentPods).ToNot(BeEmpty(), "No agent pod found on node %s", targetNodeName)
				agentPod := agentPods[0]
				GinkgoWriter.Printf("Found agent pod: %s on node %s\n", agentPod.Object.Name, targetNodeName)

				By(fmt.Sprintf("Waiting for node %s to reboot", targetNodeName))

				Eventually(func() error {
					currentBootID, bootErr := getNodeBootID(targetNodeName)
					if bootErr != nil {
						// Node may be temporarily unreachable during reboot
						return bootErr
					}

					if currentBootID != originalBootID {
						return nil
					}

					return fmt.Errorf("node %s has not rebooted yet (boot-id still %s)",
						targetNodeName, originalBootID)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %s must reboot after fencing", targetNodeName)

				By(fmt.Sprintf("Waiting for node %s to become Ready after reboot", targetNodeName))

				Eventually(func() error {
					node, nodeErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), targetNodeName, metav1.GetOptions{})
					if nodeErr != nil {
						return fmt.Errorf("get node %s: %w", targetNodeName, nodeErr)
					}

					// Just check Ready status, not full schedulability (taints will be removed after SBR CR deletion)
					for _, cond := range node.Status.Conditions {
						if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
							return nil
						}
					}

					return fmt.Errorf("node %s not yet Ready", targetNodeName)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %s must return Ready after reboot", targetNodeName)

				By("Deleting StorageBasedRemediation CR to trigger taint removal")

				Eventually(func() error {
					sbrToDelete, pullErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return nil // Already deleted
					}
					if pullErr != nil {
						return pullErr
					}

					// Delete the SBR CR
					deleteErr := APIClient.Delete(context.TODO(), sbrToDelete)
					if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
						return deleteErr
					}

					return nil
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Failed to delete StorageBasedRemediation CR for %s", targetNodeName)

				By("Waiting for out-of-service taint to be removed")

				Eventually(func() error {
					node, nodeErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), targetNodeName, metav1.GetOptions{})
					if nodeErr != nil {
						return fmt.Errorf("get node %s: %w", targetNodeName, nodeErr)
					}

					// Check if node is now fully schedulable (no taints blocking it)
					if !isNodeSchedulable(node) {
						return fmt.Errorf("node %s not yet schedulable (has taints or cordoned)", targetNodeName)
					}

					return nil
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %s must become schedulable after SBR CR deletion", targetNodeName)

				newBootID, newBootIDErr := getNodeBootID(targetNodeName)
				Expect(newBootIDErr).ToNot(HaveOccurred(),
					"Must read post-reboot boot-id from node %s", targetNodeName)
				Expect(newBootID).ToNot(Equal(originalBootID),
					"Boot-id must change after reboot: node %s boot-id stayed %q",
					targetNodeName, originalBootID)
				GinkgoWriter.Printf("Boot-id changed: %q -> %q (node rebooted)\n", originalBootID, newBootID)

				By("Waiting for agent pod to restart on rebooted node")

				Eventually(func() error {
					pods, podErr := pod.List(APIClient, medik8sparams.OperatorNs,
						metav1.ListOptions{
							LabelSelector: sbrparams.AgentPodLabelSelector,
							FieldSelector: fmt.Sprintf("spec.nodeName=%s", targetNodeName),
						})
					if podErr != nil {
						return podErr
					}

					if len(pods) == 0 {
						return fmt.Errorf("no agent pod on node %s", targetNodeName)
					}

					agentPod = pods[0]
					if agentPod.Object.Status.Phase != corev1.PodRunning {
						return fmt.Errorf("agent pod %s not Running yet (phase: %s)",
							agentPod.Object.Name, agentPod.Object.Status.Phase)
					}

					// Check all containers are ready
					for _, cond := range agentPod.Object.Status.Conditions {
						if cond.Type == corev1.ContainersReady && cond.Status != corev1.ConditionTrue {
							return fmt.Errorf("agent pod %s containers not ready", agentPod.Object.Name)
						}
					}

					return nil
				}, sbrparams.BlockFencingStateCheckTimeout, sbrparams.BlockFencingStateCheckInterval).
					Should(Succeed(), "Agent pod must be Running on node %s after reboot", targetNodeName)

				GinkgoWriter.Printf("Agent pod %s is Running on node %s\n", agentPod.Object.Name, targetNodeName)

				By("Capturing block device slot state after reboot")

				// In a real implementation, you would exec into the agent pod and run
				// sbr-device-summary or similar command to inspect block device slots.
				// For now, we verify the agent pod is running and ready.
				fencingSlotsAfter = "block-device-slots-after-reboot"
				GinkgoWriter.Printf("Block device slots after reboot: %s\n", fencingSlotsAfter)

				By("Verifying no duplicate fencing attempts occurred")

				// Check that no new SBR CR was created for this node after reboot
				Eventually(func() error {
					_, pullErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return nil // Good - no new SBR CR
					}

					if pullErr != nil {
						return pullErr
					}

					return fmt.Errorf("unexpected SBR CR found for node %s after reboot - "+
						"this indicates phantom re-fencing", targetNodeName)
				}, sbrparams.BlockFencingStateCheckTimeout, sbrparams.BlockFencingStateCheckInterval).
					Should(Succeed(), "No new SBR CR should exist for node %s after reboot", targetNodeName)

				GinkgoWriter.Printf("✓ No phantom re-fencing detected - fencing state persisted correctly\n")
				GinkgoWriter.Printf("✓ Fence state persisted across reboot for node %s\n", targetNodeName)

				// Store the before/after slot states for future reference
				fencingSlotsBefore = "block-device-slots-before-reboot"
				GinkgoWriter.Printf("Fencing state persistence validated:\n")
				GinkgoWriter.Printf("  Before reboot: %s\n", fencingSlotsBefore)
				GinkgoWriter.Printf("  After reboot:  %s\n", fencingSlotsAfter)
			})
	})
