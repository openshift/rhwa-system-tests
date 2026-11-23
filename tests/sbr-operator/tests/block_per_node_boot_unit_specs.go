package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("SBR BlockPerNode boot unit", Serial,
	Label(labels.OperatorSBR, labels.TierResiliency, labels.TierPreview, labels.FeatureBlockPerNode,
		labels.ComponentRemediation, labels.DisruptionDestructive, labels.PlatformAny, labels.FrequencyWeekly), func() {
		It("RHWA-1839-21 Autonomous reboot without a CR - The local agent releases the rejoin cordon",
			reportxml.ID("RHWA-1839-21"), FlakeAttempts(1), func() {
				test := newBPNFixture("RHWA-1839-21")
				test.enableBootUnit()

				By("Enable the boot service. Pause NHC and check the worker baseline.")
				test.prepareNodesAndCreateSBRConfig()
				test.createPausedNHCAndSBRTemplate()
				test.waitForAgentReporterAndStorageReadiness()
				test.requireInstalledBootUnit()
				test.requireKnownHistoryAndObserveRecovery()

				By("Block target storage and API access. Observe an autonomous host reboot.")
				outageAt := time.Now()
				storage, api, combinedAt := test.applyStorageLossThenAPILoss()
				boot, rebootAt := test.waitForTargetHostBootIDChange(combinedAt)
				test.nodes[0].boot = boot

				By("Restore API access while the boot service runs. Check the rejoin cordon and kubelet start order.")
				apiRestoreAt := time.Now()
				test.restoreAPIWhileBootUnitRuns(api)
				cordonedAt := test.waitForBootUnitCordon(apiRestoreAt)
				test.waitForBootUnitAndKubelet(false)
				test.assertNetworkFaultsPersistAfterReboot(storage)
				test.waitForReadyNodesWithExpectedBootIDs()

				By("Create a target-only affinity Pod. Require the scheduler to report it unschedulable.")
				test.createWorkloadPod(true)
				test.waitForWorkloadSchedulingBlocked()

				By("Restore storage. Require the rejoin cordon to remain until the recovery period can end.")
				storageRestoreAt := time.Now()
				earliestRelease := storageRestoreAt.Add(test.recoveryPeriod)
				test.removeNetworkFault(storage)
				test.waitUntil(earliestRelease)

				By("Require local release and scheduling of the same Pod without a CR or OutOfService taint.")
				test.waitForWorkerRecovery(corev1.ConditionFalse)
				test.waitForWorkloadScheduledOnTarget()
				through := time.Now()
				test.waitForCompleteAPIHistory(outageAt, through, true)
				target := test.nodes[0]
				samples := target.recorder.Snapshot().Observations
				Expect(bpnCheckRejoinHistory(samples, target.initial.UID, cordonedAt, through, earliestRelease)).To(Succeed())
				var healthyAt time.Time
				for _, sample := range samples {
					if sample.UID == target.initial.UID && !sample.At.Before(storageRestoreAt) && !sample.At.After(through) &&
						bpnStorageUnhealthyStatus(sample.Object) == corev1.ConditionFalse {
						healthyAt = sample.At

						break
					}
				}
				Expect(healthyAt).NotTo(BeZero(), "The watch must see a fresh healthy storage report after restoration")
				Expect(bpnHealthInterval(samples, target.initial.UID, corev1.ConditionFalse, healthyAt, through)).To(Succeed())
				Expect(test.checkNoRequestsDuringRejoin(outageAt)).To(Succeed())
				Expect(test.checkRecordedBootIDsUnchanged(rebootAt, 0)).To(Succeed())
				Expect(test.checkRecordedBootIDsUnchanged(outageAt, test.peerIndices()...)).To(Succeed())
			})

		It("RHWA-1839-22 NHC claims a rejoin cordon - The peer releases it while storage remains failed",
			reportxml.ID("RHWA-1839-22"), FlakeAttempts(1), func() {
				test := newBPNFixture("RHWA-1839-22")
				test.enableBootUnit()

				By("Enable the boot service. Pause NHC and confirm the original Pod runs on the target.")
				test.prepareNodesAndCreateSBRConfig()
				test.createPausedNHCAndSBRTemplate()
				test.waitForAgentReporterAndStorageReadiness()
				test.requireInstalledBootUnit()
				test.createNamespaceAndWaitForPinnedWorkloadPod()
				test.requireKnownHistoryAndObserveRecovery()

				By("Block target storage and API access. Observe an autonomous host reboot.")
				outageAt := time.Now()
				storage, api, combinedAt := test.applyStorageLossThenAPILoss()
				boot, rebootAt := test.waitForTargetHostBootIDChange(combinedAt)
				test.nodes[0].boot = boot

				By("Restore API access while the boot service runs. Check the rejoin cordon without a CR.")
				apiRestoreAt := time.Now()
				test.restoreAPIWhileBootUnitRuns(api)
				cordonedAt := test.waitForBootUnitCordon(apiRestoreAt)
				test.waitForBootUnitAndKubelet(false)
				test.assertNetworkFaultsPersistAfterReboot(storage)
				test.waitForReadyNodesWithExpectedBootIDs()
				test.waitForCompleteAPIHistory(outageAt, time.Now(), false)
				Expect(test.checkNoRequestsDuringRejoin(outageAt)).To(Succeed())

				By("Resume NHC. Require its CR, OutOfService, and eviction of the original Pod.")
				test.setNHCRequestCreationPaused(false)
				request, requestAt := test.waitForNHCOwnedSBRCR()
				test.waitForRequestedTargetBoot(request, requestAt, rebootAt)
				result := test.waitForOOSPolicySuccess(request, requestAt)
				test.waitForOriginalWorkloadEviction(result)
				test.assertNetworkFaultsPersistAfterReboot(storage)
				Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
					node, err := test.getNode(ctx, 0)
					if err != nil {
						return err
					}
					keyed, pending := bpnRejoinState(node)
					if !keyed || pending != corev1.ConditionTrue || !node.Spec.Unschedulable ||
						bpnOutOfService(node) != "nodeshutdown:NoExecute" {
						return fmt.Errorf("node %s must retain its rejoin restrictions and OutOfService while claimed", node.Name)
					}

					return nil
				})).To(Succeed())

				By("Pause NHC and delete the CR. Require peer release while target storage remains failed.")
				test.setNHCRequestCreationPaused(true)
				deletionDispatch := time.Now()
				test.deleteSBRAndWaitForBaselineRestrictions(request)
				test.waitForCompleteAPIHistory(cordonedAt, deletionDispatch, false)
				target := test.nodes[0]
				Expect(bpnCheckRejoinHistory(target.recorder.Snapshot().Observations, target.initial.UID,
					cordonedAt, deletionDispatch, time.Time{})).To(Succeed())
				test.waitForWorkerRecovery(corev1.ConditionTrue)
				test.assertNetworkFaultsPersistAfterReboot(storage)

				By("Restore storage. Check full worker recovery and unchanged peer boot IDs and restrictions.")
				test.removeNetworkFault(storage)
				test.waitForWorkerRecovery(corev1.ConditionFalse)
				test.waitForCompleteAPIHistory(outageAt, time.Now(), false)
				Expect(test.checkNoSBRsOrRestrictionChanges(outageAt, test.peerIndices()...)).To(Succeed())
				Expect(test.checkRecordedBootIDsUnchanged(outageAt, test.peerIndices()...)).To(Succeed())
			})
	})
