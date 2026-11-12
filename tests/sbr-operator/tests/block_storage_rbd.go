package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
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
			targetNodeName   string
			targetNodeID     uint16
			observerPodName  string
			setupSBRC        *unstructured.Unstructured
			originalBootID   string
			postRebootBootID string
		)

		BeforeAll(func() {
			By("Clearing residual Ceph OSD blocklist entries from prior tests")
			clearCephOSDBlocklist()

			// An NHC remediating through the SBR template would create its own
			// StorageBasedRemediation CR for this node and break the "exactly once" assertion.
			requireNoSBRNodeHealthCheck()
			requireSBRCBlockVolumeModeSupport()
			setupSBRC = setupBlockModeSBRC(sbrparams.SBRCBlockPersistentFencingTestName)

			By("Asserting the SBRC reports concurrent-writeable shared storage")

			Eventually(func() (bool, error) {
				return sbrcConcurrentWriteable(sbrparams.SBRCBlockPersistentFencingTestName)
			}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
				"SBRC %q must report status.storageValidation.concurrentWriteable=true before fencing "+
					"can proceed", sbrparams.SBRCBlockPersistentFencingTestName)

			By("Selecting target worker node (schedulable, not controller pod host)")

			targetNodeName = pickTargetWorkerNode()
			if targetNodeName == "" {
				Skip("No schedulable worker node available (excluding controller nodes); " +
					"skipping block persistent fencing test")
			}

			GinkgoWriter.Printf("Target node: %s\n", targetNodeName)

			By("Selecting an observer agent pod on a node other than the target")

			var observerErr error

			observerPodName, observerErr = agentPodNameByNode(
				sbrparams.SBRCBlockPersistentFencingTestName, targetNodeName, false)
			Expect(observerErr).ToNot(HaveOccurred(),
				"An agent pod outside node %q must be available to read the shared device; "+
					"fencing state cannot be observed from the fenced node itself", targetNodeName)
			GinkgoWriter.Printf("Observer agent pod: %q\n", observerPodName)

			By("Resolving the target node's Block-mode node ID from the on-device node map")

			nodeMap, jsonPayload, mapErr := readNodeMapFromPod(observerPodName)
			Expect(mapErr).ToNot(HaveOccurred(),
				"Must read the node map through observer pod %q (payload: %s)",
				observerPodName, jsonPayload)
			entry, mapped := nodeMap.Entries[targetNodeName]
			Expect(mapped).To(BeTrue(),
				"Node map must assign a node ID to target node %q (payload: %s)",
				targetNodeName, jsonPayload)
			targetNodeID = entry.NodeID
			Expect(targetNodeID).ToNot(BeZero(),
				"Target node %q must have a non-zero Block-mode node ID", targetNodeName)
			GinkgoWriter.Printf("Target node %q owns node ID %d (fence slot index %d)\n",
				targetNodeName, targetNodeID, targetNodeID-1)
		})

		AfterAll(func() {
			By("Clearing any residual Ceph OSD blocklist entries from fencing")
			clearCephOSDBlocklist()

			if targetNodeName != "" {
				By("Force-deleting any leftover StorageBasedRemediation CR for target node")
				cleanupSBRCR(context.Background(), targetNodeName)
			}

			if setupSBRC != nil {
				By("Deleting SBRC created for block persistent fencing test")
				deleteSBRCAndWait(setupSBRC, sbrparams.SBRCBlockPersistentFencingTestName)
			}

			if targetNodeName != "" {
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
			reportxml.ID("1916"),
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
					cleanupSBRCR(context.Background(), targetNodeName)
				})

				// Start periodic node status logging in background
				ctx, cancel := context.WithCancel(context.Background())
				DeferCleanup(func() {
					cancel()
				})

				go func() {
					ticker := time.NewTicker(30 * time.Second)
					defer ticker.Stop()

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

				By("Capturing block device fence slot state before node reboot")

				var fenceBefore FenceSlot

				Eventually(func() error {
					hood, readErr := readFenceNeighbourhood(observerPodName, targetNodeID)
					if readErr != nil {
						return readErr
					}

					var fenceErr error
					fenceBefore, fenceErr = hood.fenceRequested()

					return fenceErr
				}, sbrparams.BlockFenceWriteTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Fence slot for node %q (node ID %d) must hold a fence request after %s=True",
					targetNodeName, targetNodeID, sbrparams.FencingSucceededCondition)
				Expect(fenceBefore.TargetNodeID).To(Equal(targetNodeID),
					"Fence message in slot index %d must target node ID %d (node %q)",
					fenceBefore.SlotIndex, targetNodeID, targetNodeName)
				GinkgoWriter.Printf("Fence slot before reboot: index=%d writer=%d target=%d reason=%d\n",
					fenceBefore.SlotIndex, fenceBefore.WriterNodeID, fenceBefore.TargetNodeID,
					fenceBefore.Reason)

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
					return nodeConditionIs(targetNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
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

					deleteErr := APIClient.Delete(context.TODO(), sbrToDelete)
					if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
						return deleteErr
					}

					return nil
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Failed to delete StorageBasedRemediation CR for %s", targetNodeName)

				By("Waiting for out-of-service taint to be removed")

				// While the CR (and therefore the taint) remains, the Ceph RBD CSI node plugin is
				// evicted; waiting for the agent before this leaves it stuck in ContainerCreating.
				Eventually(func() error {
					return nodeRestoredAfterRemediation(targetNodeName)
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %s must become schedulable after SBR CR deletion", targetNodeName)

				var newBootIDErr error
				postRebootBootID, newBootIDErr = getNodeBootID(targetNodeName)
				Expect(newBootIDErr).ToNot(HaveOccurred(),
					"Must read post-reboot boot-id from node %s", targetNodeName)
				Expect(postRebootBootID).ToNot(Equal(originalBootID),
					"Boot-id must change after reboot: node %s boot-id stayed %q",
					targetNodeName, originalBootID)
				GinkgoWriter.Printf("Boot-id changed: %q -> %q (node rebooted)\n",
					originalBootID, postRebootBootID)

				// ODF blocklists the victim's IP range in Ceph while the taint is on, and does not
				// always lift it once the taint is gone (DFBUGS-11731). Until it is lifted, the
				// victim's RBD map fails with ret=-108 and its agent never becomes Ready.
				By("Clearing the Ceph OSD blocklist left on the target by the fencing")
				clearCephOSDBlocklist()

				By("Waiting for agent pod to restart on rebooted node")

				Eventually(func() error {
					return agentPodReadyOnNode(sbrparams.SBRCBlockPersistentFencingTestName, targetNodeName)
				}, sbrparams.BlockAgentReadyAfterRebootTimeout, sbrparams.BlockFencingStateCheckInterval).
					Should(Succeed(),
						"The SBRC %q agent pod on node %q must become Ready after it rebooted",
						sbrparams.SBRCBlockPersistentFencingTestName, targetNodeName)

				By("Capturing block device fence slot state after reboot")

				// The rebooted agent disarms its own fence slot on startup (FENCE_REASON_NONE or
				// empty). Seeing that through the observer proves the on-device fencing state was
				// read back correctly rather than left as a live fence request that would drive
				// phantom re-fencing.
				Eventually(func() error {
					hood, readErr := readFenceNeighbourhood(observerPodName, targetNodeID)
					if readErr != nil {
						return readErr
					}

					return hood.ownSlotQuiet()
				}, sbrparams.BlockFencingStateCheckTimeout, sbrparams.BlockFencingStateCheckInterval).
					Should(Succeed(),
						"Fence slot for node %q (node ID %d) must be quiet after the agent restarts: "+
							"empty or FENCE_REASON_NONE", targetNodeName, targetNodeID)
				GinkgoWriter.Printf("Fence slot after reboot is quiet for node %q (node ID %d)\n",
					targetNodeName, targetNodeID)

				By(fmt.Sprintf("Consistently asserting for %s that no duplicate fencing occurs "+
					"(stable boot-id, Ready, no StorageBasedRemediation CR)",
					sbrparams.BlockNoFencingCheckDuration))

				Consistently(func() error {
					return victimUnfenced(targetNodeName, postRebootBootID)
				}, sbrparams.BlockNoFencingCheckDuration, sbrparams.BlockNoFencingCheckInterval).
					Should(Succeed(),
						"Node %q must not be re-fenced after recovery: boot-id must stay %q, "+
							"it must stay Ready, and no StorageBasedRemediation CR may appear",
						targetNodeName, postRebootBootID)

				GinkgoWriter.Printf("✓ No phantom re-fencing detected - fencing state persisted correctly\n")
				GinkgoWriter.Printf("✓ Fence state persisted across reboot for node %s\n", targetNodeName)
			})
	})
