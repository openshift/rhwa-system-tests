package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD StorageBasedRemediationConfig Lifecycle",
	Ordered,
	ContinueOnFailure,
	Label(
		labels.OperatorSBR,
		labels.TierAcceptance,
		labels.FrequencyNightly,
		labels.DisruptionNonDestructive,
		labels.PlatformAny,
		labels.ComponentController,
	), func() {
		var rbdStorageClass string

		// The two SBRCs never run at the same time: every agent holds /dev/watchdog, so a second
		// agent DaemonSet on the same nodes would crash-loop on EBUSY. B is created only after A
		// and its DaemonSet are gone.
		lifecycleSBRCNames := []string{
			sbrparams.SBRCBlockLifecycleTestNameA,
			sbrparams.SBRCBlockLifecycleTestNameB,
		}

		// The storage-class-less SBRC is admitted on a cluster with no validating webhook, so a
		// crashed run can leave it behind. It is pre-cleaned alongside A and B: were it still there,
		// its create would come back AlreadyExists instead of exercising the validation at all.
		preCleanSBRCNames := append([]string{sbrparams.SBRCBlockNoStorageClassTestName},
			lifecycleSBRCNames...)

		BeforeAll(func() {
			By("Verifying Self Node Remediation is not running")
			failIfSNRRunning()

			By("Verifying the installed SBR operator supports sharedStorageVolumeMode")
			requireSBRCBlockVolumeModeSupport()

			By("Discovering the Ceph RBD block storage class")

			rbdStorageClass = discoverCephRBDStorageClass()

			for _, staleName := range preCleanSBRCNames {
				By(fmt.Sprintf("Removing any SBRC %q left by an earlier run", staleName))
				deleteSBRCAndWait(buildSBRC(staleName, map[string]interface{}{}), staleName)

				By(fmt.Sprintf("Waiting for the agent DaemonSet of %q to be garbage-collected", staleName))
				waitForDaemonSetGC(sbrparams.SBRAgentDaemonSetPrefix + staleName)
			}
		})

		AfterAll(func() {
			for _, leftoverName := range lifecycleSBRCNames {
				By(fmt.Sprintf("Removing any leftover SBRC %q", leftoverName))
				deleteSBRCAndWait(buildSBRC(leftoverName, map[string]interface{}{}), leftoverName)
			}
		})

		It("Block-mode SBRC lifecycle on Ceph RBD: create, patch, replace and delete",
			reportxml.ID("1960"),
			func() {
				By("Step 1: Asserting a Block-mode SBRC without sharedStorageClass never gets agents")

				// Block mode has no fallback storage: without a sharedStorageClass there is no RWX
				// Block PVC to coordinate through, so the config must never reach the agents. The
				// rule is enforced in both the admission webhook and the controller, so either layer
				// catching it is a pass.
				noStorageClassSBRC := buildSBRC(sbrparams.SBRCBlockNoStorageClassTestName,
					map[string]interface{}{"sharedStorageVolumeMode": "Block"})

				DeferCleanup(func() {
					By("DeferCleanup: removing the storage-class-less SBRC if admission let it through")
					deleteSBRCAndWait(noStorageClassSBRC.DeepCopy(),
						sbrparams.SBRCBlockNoStorageClassTestName)
				})

				assertInvalidSBRCNeverBecomesOperational(noStorageClassSBRC,
					sbrparams.SBRCBlockNoStorageClassTestName, "sharedStorageClass")

				By(fmt.Sprintf("Step 2: Creating Block-mode SBRC %q on Ceph RBD",
					sbrparams.SBRCBlockLifecycleTestNameA))

				sbrcA := buildSBRC(sbrparams.SBRCBlockLifecycleTestNameA,
					buildBlockModeSBRC(rbdStorageClass, map[string]interface{}{
						"sbrTimeoutSeconds": int64(sbrparams.SBRCTimeoutSecondsMin),
					}))
				Expect(APIClient.Create(context.TODO(), sbrcA)).To(Succeed(),
					"StorageBasedRemediationConfig %q must be created",
					sbrparams.SBRCBlockLifecycleTestNameA)
				uidA := sbrcA.GetUID()
				dsNameA := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCBlockLifecycleTestNameA

				By("Step 3: Asserting the shared storage PVC is ReadWriteMany with volumeMode Block")

				pvcA := waitForSBRCPVCBound(sbrparams.SBRCBlockLifecycleTestNameA, uidA)
				Expect(pvcA.Spec.AccessModes).To(ContainElement(corev1.ReadWriteMany),
					"PVC %s must be ReadWriteMany: every agent writes to the same device", pvcA.Name)
				Expect(pvcA.Spec.VolumeMode).ToNot(BeNil(),
					"PVC %s must declare a volumeMode in Block mode", pvcA.Name)
				Expect(*pvcA.Spec.VolumeMode).To(Equal(corev1.PersistentVolumeBlock),
					"PVC %s must be a raw block volume, not a filesystem one", pvcA.Name)

				By("Step 4: Waiting for the Block device-init Job to succeed")

				waitForBlockInitJobSucceeded(sbrparams.SBRCBlockLifecycleTestNameA, uidA)

				// Read while the Job is still there: it is TTL-collected after it finishes, and
				// step 6 needs its completion time.
				initCompletedAt, completionErr := blockInitJobCompletionTime(
					sbrparams.SBRCBlockLifecycleTestNameA, uidA)
				Expect(completionErr).ToNot(HaveOccurred(),
					"The Block device-init Job's completion time must be readable right after it succeeds")

				By("Step 5: Waiting for the agent DaemonSet to be ready")

				waitForSBRCReady(sbrparams.SBRCBlockLifecycleTestNameA)
				agentDSA, dsErr := sbrcAgentDaemonSet(sbrparams.SBRCBlockLifecycleTestNameA)
				Expect(dsErr).ToNot(HaveOccurred())

				By("Step 6: Asserting the agent DaemonSet was created only after the init Job succeeded")

				// The operator gates the DaemonSet on the init Job: agents must never open a device
				// that has not been formatted with a valid superblock.
				Expect(agentDSA.CreationTimestamp.Time).ToNot(BeTemporally("<", initCompletedAt),
					"DaemonSet %s was created at %s, before the Block device-init Job completed at %s: "+
						"agents must not start against an unformatted device",
					dsNameA, agentDSA.CreationTimestamp.Time, initCompletedAt)

				By("Step 7: Asserting the shared storage is attached as a raw device, not mounted")

				assertBlockModeSharedStorageAttached(agentDSA, pvcA.Name)

				By("Step 8: Asserting the agent args select the raw device with file locking disabled")

				assertBlockModeAgentArgs(agentDSA)

				By("Step 9: Asserting the watchdog was moved to the side mount instead of /dev")

				assertBlockModeWatchdogMount(agentDSA, sbrparams.DefaultWatchdogPath)

				By("Step 10: Asserting sharedStorageVolumeMode cannot be changed after creation")

				// Switching modes in place would leave the agents reading a device laid out for the
				// other mode; the only supported path is deleting the config and its PVC.
				//
				// Unlike the storage-class rule in step 1, immutability is enforced only by the
				// admission webhook - the controller does not re-check it at reconcile. So with no
				// webhook registered there is nothing to assert, and attempting the patch anyway
				// would land it and leave the rest of this spec running against an SBRC that is no
				// longer in Block mode.
				if sbrcAdmissionWebhookRegistered() {
					expectAdmissionDenial(
						patchSBRCSpec(sbrparams.SBRCBlockLifecycleTestNameA, map[string]interface{}{
							"sharedStorageVolumeMode": "Filesystem",
						}),
						sbrparams.SBRCBlockVolumeModeField,
						fmt.Sprintf("Patching sharedStorageVolumeMode of SBRC %q to Filesystem must "+
							"be rejected: the volume mode is immutable after creation",
							sbrparams.SBRCBlockLifecycleTestNameA))

					// The denial is only half the assertion: what matters is that nothing moved.
					// Re-read rather than trust the error, so a webhook that rejects the response
					// after the write has already landed is caught here.
					storedMode, modeErr := sbrcVolumeMode(sbrparams.SBRCBlockLifecycleTestNameA)
					Expect(modeErr).ToNot(HaveOccurred())
					Expect(storedMode).To(Equal("Block"),
						"SBRC %q must still be in Block mode after the rejected patch",
						sbrparams.SBRCBlockLifecycleTestNameA)
				} else {
					GinkgoWriter.Printf("Skipping the sharedStorageVolumeMode immutability assertion: "+
						"no validating admission webhook covers %s on this cluster, and the rule has "+
						"no reconcile-time counterpart\n", sbrparams.SBRCResourcePlural)
				}

				By("Step 11: Patching sbrTimeoutSeconds and waiting for the DaemonSet to roll out")

				prePatchGeneration := agentDSA.Generation
				Expect(patchSBRCSpec(sbrparams.SBRCBlockLifecycleTestNameA, map[string]interface{}{
					"sbrTimeoutSeconds": int64(sbrparams.SBRCTimeoutSecondsMin + 10),
				})).To(Succeed(),
					"Patching sbrTimeoutSeconds of SBRC %q must be accepted",
					sbrparams.SBRCBlockLifecycleTestNameA)
				waitForDaemonSetRollout(dsNameA, prePatchGeneration)

				By(fmt.Sprintf("Steps 12-13: Deleting SBRC %q, asserting no node map cleanup Job runs, "+
					"and waiting for its DaemonSet and PVC to go",
					sbrparams.SBRCBlockLifecycleTestNameA))

				// Step 13 is folded into the delete: Block mode keeps its node map inside the
				// device's double-buffered region, not in a filesystem file, so the RHWA-1056
				// cleanup Job has nothing to wipe and must never run. That Job lives and dies inside
				// the finalizer drain, so the watch for it has to start with the delete call.
				deleteSBRCAssertingNoCleanupJob(sbrcA.DeepCopy(), sbrparams.SBRCBlockLifecycleTestNameA)
				waitForDaemonSetGC(dsNameA)
				waitForSBRCPVCGone(sbrparams.SBRCBlockLifecycleTestNameA, uidA)

				By(fmt.Sprintf("Step 14: Creating Block-mode SBRC %q on Ceph RBD",
					sbrparams.SBRCBlockLifecycleTestNameB))

				sbrcB := buildSBRC(sbrparams.SBRCBlockLifecycleTestNameB,
					buildBlockModeSBRC(rbdStorageClass, map[string]interface{}{
						"sbrTimeoutSeconds": int64(sbrparams.SBRCTimeoutSecondsMin),
					}))
				Expect(APIClient.Create(context.TODO(), sbrcB)).To(Succeed(),
					"StorageBasedRemediationConfig %q must be created",
					sbrparams.SBRCBlockLifecycleTestNameB)
				uidB := sbrcB.GetUID()
				dsNameB := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCBlockLifecycleTestNameB

				By("Step 15: Asserting the second SBRC gets its own PVC and its own init Job")

				pvcB := waitForSBRCPVCBound(sbrparams.SBRCBlockLifecycleTestNameB, uidB)
				Expect(pvcB.Name).ToNot(Equal(pvcA.Name),
					"SBRC %q must provision its own shared-storage PVC rather than reuse the deleted one's",
					sbrparams.SBRCBlockLifecycleTestNameB)
				Expect(pvcB.UID).ToNot(Equal(pvcA.UID),
					"The PVC of SBRC %q must be a new object, not the one left by %q",
					sbrparams.SBRCBlockLifecycleTestNameB, sbrparams.SBRCBlockLifecycleTestNameA)
				waitForBlockInitJobSucceeded(sbrparams.SBRCBlockLifecycleTestNameB, uidB)

				// Owner-matched, so this proves the operator ran a fresh init for B rather than
				// letting the agents come up against whatever the deleted SBRC left behind.
				_, initJobErr := sbrcOwnedJob(uidB)
				Expect(initJobErr).ToNot(HaveOccurred(),
					"SBRC %q must own its own Block device-init Job", sbrparams.SBRCBlockLifecycleTestNameB)

				By("Step 16: Waiting for the second SBRC's agents to be ready")

				waitForSBRCReady(sbrparams.SBRCBlockLifecycleTestNameB)

				By(fmt.Sprintf("Step 17: Deleting SBRC %q and waiting for its DaemonSet and PVC to go",
					sbrparams.SBRCBlockLifecycleTestNameB))

				deleteSBRCAndWait(sbrcB.DeepCopy(), sbrparams.SBRCBlockLifecycleTestNameB)
				waitForDaemonSetGC(dsNameB)
				waitForSBRCPVCGone(sbrparams.SBRCBlockLifecycleTestNameB, uidB)
			})
	})
