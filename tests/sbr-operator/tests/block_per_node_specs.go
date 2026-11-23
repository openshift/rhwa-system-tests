//nolint:godox,lll // Keep the approved TODO assertions and step strings unchanged.
package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("SBR BlockPerNode", Serial,
	Label(labels.OperatorSBR, labels.TierResiliency, labels.TierPreview, labels.FeatureBlockPerNode,
		labels.ComponentRemediation,
		labels.DisruptionDestructive, labels.PlatformAny, labels.FrequencyWeekly), func() {
		// TODO(RHWA-1839-02): Repeat during an actual API server outage. This needs an independent control-plane recovery procedure and independent API-event evidence.
		It("RHWA-1839-02 API loss with healthy storage - No worker reboots", reportxml.ID("RHWA-1839-02"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-02")

			By("Complete the baseline and confirm that all observers are ready.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Block API access on one worker. Confirm transport failure while storage remains healthy.")
			targetFault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockAPILoss, false)
			test.applyAndConfirmNetworkFault(targetFault)

			By("Observe unchanged boot IDs, healthy storage, and no CR or restriction for 300 seconds.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Restore the first worker's API access before the all-worker fault.")
			test.removeNetworkFault(targetFault)
			test.waitForWorkerRecovery(corev1.ConditionFalse)

			By("Block API access on all selected workers. Confirm each transport failure.")
			test.applyAndConfirmNetworkFault(targetFault)
			for _, index := range test.peerIndices() {
				fault := test.prepareAndTrackNetworkFault(index, sbrutils.BlockAPILoss, false)
				test.applyAndConfirmNetworkFault(fault)
			}

			By("Observe unchanged boot IDs, healthy storage, and no CR or restriction for another 300 seconds.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Restore API access on all workers. Require full worker recovery.")
			test.removeTrackedNetworkFaults()
			test.waitForWorkerRecovery(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-03): Count every target boot before SSH returns. This needs a trusted host boot counter and does not affect the initial result.
		It("RHWA-1839-03 Broad storage loss - NHC remediates only one worker", reportxml.ID("RHWA-1839-03"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-03")

			By("Complete the baseline with active NHC, minHealthy=N-1, and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Prepare storage faults on all workers. Make the target fault persistent across reboot.")
			faults := make([]*sbrutils.BlockNetworkFault, 0, len(test.nodes))
			for index := range test.nodes {
				faults = append(faults, test.prepareAndTrackNetworkFault(index, sbrutils.BlockStorageLoss, index == 0))
			}
			peers := test.peerIndices()
			peersRestored := false
			restorePeers := func() {
				if peersRestored {
					return
				}
				for _, index := range peers {
					test.removeNetworkFault(faults[index])
				}
				test.waitForHealthyStorage(peers...)
				peersRestored = true
			}
			DeferCleanup(func() {
				defer func() {
					if !peersRestored {
						bpnRecoveryFailed = true
					}
				}()
				restorePeers()
			})

			By("Reject storage traffic only on worker A. Require SBRStorageUnhealthy=True within HEALTH_CHANGE.")
			test.applyAndConfirmNetworkFault(faults[0])
			test.waitForStorageUnhealthy(0)

			By("Observe A's NHC-created SBR CR within REQUEST_CREATE. Record its UID before disrupting any peer.")
			request, from := test.waitForNHCOwnedSBRCR()
			checkPeers := func() error {
				if err := test.checkRecordedBootIDsUnchanged(test.baseline, peers...); err != nil {
					return err
				}

				return test.checkNoSBRsOrRestrictionChanges(test.baseline, peers...)
			}
			Expect(checkPeers()).To(Succeed(), "No peer may receive a request or reboot")

			By("Reject storage traffic on all peers. Preserve API and SSH access on all workers.")
			for _, index := range peers {
				test.applyAndConfirmNetworkFault(faults[index])
			}
			outageDeadline, err := bpnStorageObservationDeadline(test.healthFaults, peers...)
			Expect(err).NotTo(HaveOccurred())

			By("Require True storage conditions on all peers within HEALTH_CHANGE. Check that no peer receives a request or reboots.")
			test.waitForStorageUnhealthyWhileChecking(checkPeers, peers...)

			By("Require A to reboot. Check OutOfService, policy success, and removal of the original Pod.")
			rebootObservedAt := test.waitForRequestedTargetBoot(request, from, test.baseline)
			result := test.waitForOOSPolicySuccess(request, from)
			test.waitForOriginalWorkloadEviction(result)

			By("Confirm that all storage faults remain active after A reboots.")
			test.assertNetworkFaultsPersistAfterReboot(faults...)

			By("Keep all faults active until the requested result and NO_REBOOT after the last peer fault are complete.")
			check := func() error {
				if err := checkPeers(); err != nil {
					return err
				}

				return test.checkRecordedBootIDsUnchanged(rebootObservedAt, 0)
			}
			if remaining := time.Until(outageDeadline); remaining > 0 {
				Consistently(check, remaining, sbrparams.BPNPoll).Should(Succeed())
			}
			test.assertCurrentHostBootIDsMatch()
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(check()).To(Succeed(), "Only A may reboot. No peer may receive a request or restriction")

			By("Restore storage on all peers. Require all peer storage conditions to be False before restoring A.")
			restorePeers()

			By("Restore storage on A. Require False storage conditions on all workers.")
			test.removeNetworkFault(faults[0])
			test.waitForHealthyStorage()
			healthyAt := time.Now()

			By("Let NHC delete A's CR. Require SBR restriction release and full worker recovery.")
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, healthyAt)
			test.waitForWorkerRecovery(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(check()).To(Succeed(), "Peer requests and further reboots must remain absent through restoration")
		})

		// TODO(RHWA-1839-04): Count every boot during the initial SSH blackout. This needs a trusted host boot counter and does not change this test's pass result.
		It("RHWA-1839-04 Combined outage with OnTimeout - Autonomous reboot and OutOfService at expiry", reportxml.ID("RHWA-1839-04"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-04")
			test.watcherGroupSize = 1
			test.enableBootUnit()

			By("Complete autonomous preparation with active NHC and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			Expect(bpnOutOfService(test.nodes[0].initial)).To(BeEmpty(), "The target must start without an OutOfService taint")
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.requireKnownHistoryAndObserveRecovery()
			originalBoot := test.nodes[0].boot
			test.requireInstalledBootUnit()

			By("Prepare persistent storage-loss and API-loss rules on the target.")
			storage := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			api := test.prepareAndTrackNetworkFault(0, sbrutils.BlockAPILoss, true)

			By("Block the target API first. Then block its storage and confirm both faults.")
			test.applyAndConfirmNetworkFault(api)
			test.waitForHealthyStorage(0)
			test.assertCurrentHostBootIDsMatch(0)
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline, 0)).To(Succeed(),
				"The target must have no SBR request or restriction before storage loss")
			test.applyAndConfirmStorageLossDuringAPILoss(storage, api)
			combinedOutageAt := storage.Evidence().ActiveAt

			By("Require SBRStorageUnhealthy=True and the NHC-owned SBR CR for the target.")
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Observe an actual changed host boot ID within REBOOT.")
			targetBoot, rebootObservedAt := test.waitForTargetHostBootIDChange(combinedOutageAt)
			test.nodes[0].boot = targetBoot

			By("Check that the boot service expires and kubelet starts while target API access remains blocked.")
			api.SetNamespacePIDs(nil)
			faultCtx, cancelFault := context.WithTimeout(test.ctx, sbrparams.BPNFaultReady)
			faultErr := api.Confirm(faultCtx)
			cancelFault()
			Expect(faultErr).NotTo(HaveOccurred(), "The host API fault must persist across reboot")
			test.waitForBootUnitAndKubelet(true)

			By("Confirm that both faults still apply after reconnection.")
			test.assertNetworkFaultsPersistAfterReboot(storage, api)
			through := time.Now()
			test.waitForCompleteAPIHistory(combinedOutageAt, through, false)
			for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
				if sample.UID == test.nodes[0].initial.UID && !sample.At.Before(combinedOutageAt) && !sample.At.After(through) {
					Expect(bpnCheckClearedRejoin(sample.Object)).To(Succeed(),
						"The boot service must not publish a rejoin cordon without API access")
				}
			}

			By("Require OutOfService and policy success at reboot grace expiry without valid boot evidence.")
			result := test.waitForOnTimeoutWithoutBootEvidence(request, requestCreatedAt, originalBoot)

			By("Require removal of the original Pod while target API access remains blocked.")
			test.waitForOriginalWorkloadEviction(result)

			By("Keep both faults active for NO_REBOOT. Require unchanged peer boot IDs and restrictions.")
			peers := test.peerIndices()
			check := func() error {
				if err := test.checkRecordedBootIDsUnchanged(rebootObservedAt, 0); err != nil {
					return err
				}
				if err := test.checkRecordedBootIDsUnchanged(test.baseline, peers...); err != nil {
					return err
				}

				return test.checkNoSBRsOrRestrictionChanges(test.baseline, peers...)
			}
			test.observeInterval(sbrparams.BPNNoReboot, test.baseline, check)
			test.assertNetworkFaultsPersistAfterReboot(storage, api)
			Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
				if _, err := test.getAndCheckSBRCR(ctx, request); err != nil {
					return err
				}
				node, err := test.getNode(ctx, 0)
				if err != nil {
					return err
				}
				if bpnOutOfService(node) != "nodeshutdown:NoExecute" {
					return fmt.Errorf("node %s lost its OutOfService taint during the combined outage", node.Name)
				}

				return nil
			})).To(Succeed(), "The same NHC request and exact OutOfService taint must remain until fault restoration")

			By("Restore storage and API access. Require False storage health on all workers.")
			test.removeTrackedNetworkFaults()
			test.waitForHealthyStorage()

			By("Let NHC delete the CR. Require baseline restrictions and full worker recovery.")
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, time.Now())
			test.waitForWorkerRecovery(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(check()).To(Succeed(), "No peer may receive a request or restriction, and no further reboot may occur")
		})

		// TODO(RHWA-1839-05): Observe a VM during the same partition. This needs an existing VM fixture and an independent workload observer.
		It("RHWA-1839-05 One worker loses cluster communication - No worker reboots", reportxml.ID("RHWA-1839-05"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-05")

			By("Complete the baseline and confirm all observers are ready.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Prepare the singleton-isolation rules with explicit storage and SSH exceptions.")
			fault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockSingletonIsolation, false)

			By("Activate the rules. Confirm blocked API and peer transport from worker A.")
			test.applyAndConfirmNetworkFault(fault)

			By("Observe actual host boot IDs and target CR events for NO_REBOOT.")
			test.observeNoRebootWithoutRequests()

			By("Remove the isolation rules. Require restored connections and False storage health.")
			test.removeTrackedNetworkFaults()
			test.waitForHealthyStorage()

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-06): Check VMI removal or CSI attachment removal. These need suitable existing workload fixtures and are independent of this initial Pod result.
		It("RHWA-1839-06 One worker loses storage - NHC requests reboot and SBR evicts the Pod", reportxml.ID("RHWA-1839-06"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-06")

			By("Complete the baseline. Start CR, Node, host, and workload observers before the fault.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Record the workload Pod UID and all host boot IDs.")
			Expect(test.workload.UID).NotTo(BeEmpty())
			test.assertCurrentHostBootIDsMatch()

			By("Reject storage traffic only on the target. Confirm that the fault will persist across reboot.")
			fault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(fault)

			By("Require SBRStorageUnhealthy=True within HEALTH_CHANGE.")
			test.waitForStorageUnhealthy(0)

			By("Observe the NHC-created SBR CR within REQUEST_CREATE. Record its UID.")
			request, from := test.waitForNHCOwnedSBRCR()

			By("Require the target to reboot within REBOOT.")
			test.waitForRequestedTargetBoot(request, from, test.baseline)

			By("Confirm that the target storage fault remains active after reboot.")
			test.assertNetworkFaultsPersistAfterReboot(fault)

			By("Require OutOfService and policy success for the original NHC request.")
			result := test.waitForOOSPolicySuccess(request, from)
			test.assertPeerHostBootIDsUnchanged()

			By("Require the original Pod UID to disappear within WORKLOAD_GONE after OutOfService.")
			test.waitForOriginalWorkloadEviction(result)

			By("Restore target storage. Require False storage health.")
			test.removeNetworkFault(fault)
			test.waitForHealthyStorage()
			healthyAt := time.Now()

			By("Let NHC delete the CR. Require SBR restriction release and full worker recovery.")
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, healthyAt)
			test.waitForWorkerRecovery(corev1.ConditionFalse)

			By("Complete common cleanup.")
		})

		It("RHWA-1839-07 Kernel panic - A watcher reports storage failure and SBR evicts the Pod", reportxml.ID("RHWA-1839-07"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-07")
			test.watcherGroupSize = 1

			By("Complete the baseline with active NHC, one target per watcher group, and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			Expect(bpnOutOfService(test.nodes[0].initial)).To(BeEmpty(), "The target must start without an OutOfService taint")
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Save /proc/sys/kernel/sysrq and /proc/sys/kernel/panic through SSH.")
			settings := &bpnHostSettings{host: test.nodes[0].host}
			test.hostSettings = settings
			settings.sysctlSaved = test.runHost("cat /proc/sys/kernel/sysrq /proc/sys/kernel/panic")
			Expect(strings.Fields(settings.sysctlSaved)).To(HaveLen(2))
			_, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo test -w /proc/sysrq-trigger")
			test.skipOrFailOnPrerequisiteError(err, "existing SysRq trigger")

			By("Set SysRq to 1. Run sync. Send SysRq s.")
			test.disrupted = true
			test.runHost("sudo sh -c 'echo 1 > /proc/sys/kernel/sysrq; sync; echo s > /proc/sysrq-trigger'")

			By("Wait SYSRQ_SETTLE. Set kernel.panic to SYSRQ_PANIC_DELAY.")
			test.waitUntil(time.Now().Add(sbrparams.BPNSysRqSettle))
			test.runHost(fmt.Sprintf("sudo sysctl -w kernel.panic=%d", int(sbrparams.BPNSysRqPanicDelay.Seconds())))

			By("Send c to /proc/sysrq-trigger through the prepared SSH connection.")
			test.assertCurrentHostBootIDsMatch(0)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline)).To(Succeed(),
				"Workers must retain baseline restrictions and receive no SBR CR before the crash")
			originalBoot := test.nodes[0].boot
			from := time.Now()
			// SysRq triggers a kernel panic. A changed boot ID establishes a reboot. The reset mechanism requires separate evidence.
			_, triggerErr := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo sh -c 'echo c > /proc/sysrq-trigger'")
			AddReportEntry("sysrq-dispatch", fmt.Sprint(triggerErr))

			By("Observe a changed actual host boot ID within REBOOT.")
			test.nodes[0].boot, _ = test.waitForTargetHostBootIDChange(from)

			By("Restore the saved settings after the target returns.")
			Expect(test.restoreHostSettings()).To(Succeed(), "The target must restore its original SysRq settings")

			By("Require recorded True storage health after the target crash.")
			unhealthyAt := test.waitForRecordedStorageUnhealthy(from)

			By("Require the NHC-owned SBR CR and exact OutOfService taint on the target.")
			request, requestObservedAt := test.waitForRecordedNHCOwnedSBRCR(from, unhealthyAt.Add(sbrparams.BPNRequestCreate))
			result := test.waitForOOSPolicySuccess(request, requestObservedAt)

			By("Require the original Pod UID to disappear after OutOfService.")
			test.waitForOriginalWorkloadEviction(result)

			By("Let NHC delete the CR. Require baseline restrictions and full worker recovery.")
			test.waitForHealthyStorage()
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, time.Now())
			test.waitForWorkerRecovery(corev1.ConditionFalse)

			By("Check worker boot IDs and peer restrictions through recovery and POST_RESTORE.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			test.assertPeerHostBootIDsUnchanged()
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline, test.peerIndices()...)).To(Succeed(),
				"Peer workers must retain baseline restrictions and receive no SBR CR")
			Expect(test.checkRecordedTargetBootChange(test.baseline, originalBoot)).To(Succeed(),
				"The target must have only one observed boot change")
			Expect(bpnCheckCrashRequestLifetime(test.crRecorder.Snapshot().Observations, test.nodes[0].initial,
				request, test.baseline, from)).To(Succeed(),
				"The target must receive only the original NHC request, and NHC must remove it")

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-08): Retain a completed remediation CR during this restart and check for no repeated host reboot. This needs one preparatory remediation cycle.
		It("RHWA-1839-08 Planned agent Pod replacement - The host does not reboot", reportxml.ID("RHWA-1839-08"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-08")

			By("Complete the baseline and confirm observers are ready.")
			test.prepareNodesAndCreateSBRConfig()
			test.createPausedNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Record the target agent Pod UID and all host boot IDs.")
			var original *corev1.Pod
			Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
				var err error
				original, err = test.findTargetAgentPod(ctx)

				return err
			})).To(Succeed())
			test.assertCurrentHostBootIDsMatch()

			By("Delete only the target agent Pod through normal Kubernetes deletion.")
			test.deleteAgentPodByUID(original)

			By("Require a different agent Pod UID on the same target Node within AGENT_READY.")
			test.waitForReadyReplacementAndHealthyStorage(original)

			By("Require the replacement agent to become Ready and the target storage condition to be False.")
			test.waitForHealthyStorage()

			By("Continue unchanged-host-boot observation for NO_REBOOT after the replacement becomes Ready.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-09): Check an existing application during the stopped-kubelet interval. This needs a workload health check independent of kubelet status.
		It("RHWA-1839-09 Kubelet stops with healthy storage - The host does not reboot", reportxml.ID("RHWA-1839-09"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-09")

			By("Complete the baseline and confirm observers are ready.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Stop target kubelet through SSH.")
			settings := &bpnHostSettings{host: test.nodes[0].host, kubeletStopped: true}
			test.hostSettings = settings
			test.disrupted = true
			_, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo systemctl stop kubelet")
			AddReportEntry("kubelet-stop-command", fmt.Sprint(err))

			By("Confirm systemctl is-active kubelet reports inactive.")
			state := test.runHost("systemctl is-active kubelet || test \"$(systemctl is-active kubelet)\" = inactive")
			Expect(state).To(ContainSubstring("inactive"))

			By("Hold kubelet inactive for NO_REBOOT. Continue host boot-ID and API observations.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Start kubelet through SSH. Confirm that it is active.")
			Eventually(func() error {
				_, startErr := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo systemctl start kubelet && systemctl is-active --quiet kubelet")

				return startErr
			}, sbrparams.BPNRelease, sbrparams.BPNPoll).Should(Succeed())
			settings.kubeletStopped = false

			By("Require the Node to return Ready. Observe POST_RESTORE.")
			test.waitForReadyNodesWithExpectedBootIDs()
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-10): Restart one peer while another remains storage-unhealthy. This needs at least three workers and a controlled peer-restart case.
		It("RHWA-1839-10 Peer storage timeouts - Peers still process the target request", reportxml.ID("RHWA-1839-10"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-10")

			By("Complete the baseline with active target-only NHC and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveTargetOnlyNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Drop storage traffic on all peers. Confirm every peer fault.")
			peers := test.peerIndices()
			peerFaults := make([]*sbrutils.BlockNetworkFault, 0, len(peers))
			for _, index := range peers {
				peerFaults = append(peerFaults, test.prepareAndTrackNetworkFault(index, sbrutils.BlockStorageTimeout, false))
			}
			for _, fault := range peerFaults {
				test.applyAndConfirmNetworkFault(fault)
			}

			By("Require unhealthy storage on every peer. Observe no CR, reboot, or restriction for 300 seconds.")
			test.observeStorageFailureWithoutRemediation(peers...)

			By("Reject target storage traffic while peer faults remain active. Require the NHC-owned target CR.")
			targetFault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(targetFault)
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Require the target reboot, OutOfService, policy success, and removal of the original Pod.")
			test.waitForRequestedTargetBoot(request, requestCreatedAt, test.baseline)
			result := test.waitForOOSPolicySuccess(request, requestCreatedAt)
			test.waitForOriginalWorkloadEviction(result)

			By("Confirm that all storage faults persist and the peer agents continue to run.")
			test.assertNetworkFaultsPersistAfterReboot(targetFault)
			test.assertNetworkFaultsPersistAfterReboot(peerFaults...)
			test.assertPeerAgentsKeepRunningWithoutRestart(requestCreatedAt, time.Now())
			Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionTrue, requestCreatedAt, time.Now(), peers...)).
				To(Succeed(), "All peer Nodes must remain storage-unhealthy through request completion")
			test.assertPeerHostBootIDsUnchanged()
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline, peers...)).
				To(Succeed(), "The peers must receive no CR or restriction")

			By("Restore storage. Require NHC to delete the CR and check full worker recovery.")
			test.removeTrackedNetworkFaults()
			test.waitForHealthyStorage()
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, time.Now())
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-11): Check a pre-existing application's availability during the partition. This needs an application-specific observer and does not establish duplicate-writer protection.
		It("RHWA-1839-11 Worker group partition with healthy storage - No worker reboots", reportxml.ID("RHWA-1839-11"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-11")

			By("Complete the baseline and confirm observers are ready.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Prepare rules for both directions of the worker-group partition.")
			faults := []*sbrutils.BlockNetworkFault{test.prepareAndTrackNetworkFault(0, sbrutils.BlockGroupPartition, false)}
			// The peers keep their connections to each other.
			for _, index := range test.peerIndices() {
				network := test.networks[index]
				network.Peers = nil
				for _, address := range test.nodes[0].initial.Status.Addresses {
					if address.Type == corev1.NodeInternalIP || address.Type == corev1.NodeExternalIP {
						network.Peers = append(network.Peers, address.Address)
					}
				}
				test.networks[index] = network
				faults = append(faults, test.prepareAndTrackNetworkFault(index, sbrutils.BlockGroupPartition, false))
			}

			By("Activate all rules. Confirm blocked cross-group transport and retained API transport.")
			for _, fault := range faults {
				test.applyAndConfirmNetworkFault(fault)
			}

			By("Observe all selected Nodes for NO_REBOOT after the last rule becomes active.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Remove all partition rules. Confirm cross-group transport returns.")
			test.removeTrackedNetworkFaults()
			test.waitForHealthyStorage()

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-15A): Retain a VMI or attachment instead of a Pod. This needs an existing suitable fixture and a reversible retention method.
		It("RHWA-1839-15A Held Pod during cleanup - CR deletion releases SBR restrictions", reportxml.ID("RHWA-1839-15A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-15A")

			By("Complete the baseline with active NHC and a Pod on the target. Add the test finalizer.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.addWorkloadHoldFinalizer()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Request normal Pod deletion and confirm that the original Pod remains with its finalizer.")
			test.deleteWorkloadPodAndWaitForFinalizerHold()

			By("Reject target storage traffic. Require unhealthy storage and an NHC-owned CR.")
			fault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(fault)
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Require OutOfService and policy success while cleanup is Observing.")
			originalBoot := test.nodes[0].boot
			result := test.waitForOOSPolicySuccess(request, requestCreatedAt)
			Expect(bpnCheckOriginalRequestBoot(result.result, originalBoot)).To(Succeed())
			Expect(bpnRetainedCleanupHistory(test.crRecorder.Snapshot().Observations, request, result.deadline)).To(Succeed())

			By("Pause NHC creation. Delete the CR while cleanup is Observing and before its deadline.")
			test.setNHCRequestCreationPaused(true)
			Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
				current, err := test.getAndCheckSBRCR(ctx, request)
				if err != nil {
					return err
				}
				phase, _, err := unstructured.NestedString(current.Object, "status", "cleanup", "phase")
				if err != nil {
					return err
				}
				if phase != "Observing" {
					return fmt.Errorf("CR %s must still be Observing, got %q", request.GetUID(), phase)
				}

				return test.checkWorkloadPodTerminatingWithFinalizer(ctx)
			})).To(Succeed())
			Expect(time.Now()).To(BeTemporally("<", result.deadline))
			test.deleteSBRAndWaitForBaselineRestrictions(request)

			By("Require CR removal and restriction release before the deadline while the original Pod remains held.")
			Expect(time.Now()).To(BeTemporally("<", result.deadline))
			Expect(test.callAPIWithNodeIdentityChecks(test.checkWorkloadPodTerminatingWithFinalizer)).
				To(Succeed(), "The original Pod must still be held after CR deletion and release")

			By("Confirm the target reboot and the unchanged peer boot IDs.")
			test.nodes[0].boot, _ = test.waitForTargetHostBootIDChange(requestCreatedAt)
			test.assertBootUnchangedBeforeRequest(originalBoot, test.baseline, requestCreatedAt)
			test.assertPeerHostBootIDsUnchanged()
			test.assertNetworkFaultsPersistAfterReboot(fault)

			By("Restore target storage and remove the test Pod finalizer. Require full worker recovery.")
			test.removeNetworkFault(fault)
			Expect(test.removeWorkloadFinalizerAndWaitForPodDeletion()).To(Succeed())
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-15B): Keep two completed CRs on the same Node and delete them separately. This needs two request cycles and checks retention of shared restrictions.
		It("RHWA-1839-15B Held Pod at cleanup expiry - SBR preserves administrator restrictions", reportxml.ID("RHWA-1839-15B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-15B")

			By("Complete the baseline with active NHC and a Pod on the target. Add the test finalizer.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.addWorkloadHoldFinalizer()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Request normal Pod deletion and confirm that the original Pod remains with its finalizer.")
			test.deleteWorkloadPodAndWaitForFinalizerHold()

			By("Add the administrator cordon and both taints. Check that the agents and reporters remain available.")
			Expect(test.setAdministratorCordonAndTaints(true)).To(Succeed())
			test.waitForAPICheck(sbrparams.BPNAgentReady, "Agents and reporters must tolerate existing OutOfService", test.checkAgentAndReporterReadiness)
			test.assertCurrentHostBootIDsMatch()

			By("Reject target storage traffic. Require unhealthy storage and an NHC-owned CR.")
			fault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(fault)
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Require a host reboot and policy success before the cleanup deadline while the Pod remains held.")
			test.waitForRequestedTargetBoot(request, requestCreatedAt, test.baseline)
			test.assertPeerHostBootIDsUnchanged()
			result := test.waitForOOSPolicySuccess(request, requestCreatedAt)
			Expect(result.publishedAt).To(BeTemporally("<", result.deadline))
			Expect(test.callAPIWithNodeIdentityChecks(test.checkWorkloadPodTerminatingWithFinalizer)).To(Succeed())
			test.assertNetworkFaultsPersistAfterReboot(fault)

			By("Keep the Pod held through the deadline. Require Incomplete or Unknown cleanup.")
			test.waitUntil(result.deadline)
			Expect(test.callAPIWithNodeIdentityChecks(test.checkWorkloadPodTerminatingWithFinalizer)).
				To(Succeed(), "The same finalizer-held Pod must remain at cleanup expiry")
			test.waitForTerminalCleanup(request, result)
			Expect(bpnRetainedCleanupHistory(test.crRecorder.Snapshot().Observations, request, result.deadline)).To(Succeed())
			Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
				current, err := test.getAndCheckSBRCR(ctx, request)
				if err != nil {
					return err
				}
				phase, _, err := unstructured.NestedString(current.Object, "status", "cleanup", "phase")
				if err != nil {
					return err
				}
				if phase != "Incomplete" && phase != "Unknown" {
					return fmt.Errorf("CR %s must report Incomplete or Unknown cleanup with a held Pod, got %q", request.GetUID(), phase)
				}

				return nil
			})).To(Succeed())

			By("Pause NHC creation and delete the CR while the original Pod and administrator restrictions remain.")
			test.setNHCRequestCreationPaused(true)
			test.deleteSBRAndWaitForBaselineRestrictions(request)
			Expect(test.callAPIWithNodeIdentityChecks(test.checkWorkloadPodTerminatingWithFinalizer)).
				To(Succeed(), "The original Pod must still be held after CR deletion and release")
			test.waitForNodeRestrictionsToMatchBaseline()

			By("Restore storage and remove the test administrator restrictions and Pod finalizer. Require full worker recovery.")
			test.removeNetworkFault(fault)
			Expect(test.setAdministratorCordonAndTaints(false)).To(Succeed())
			Expect(test.removeWorkloadFinalizerAndWaitForPodDeletion()).To(Succeed())
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-16A): Check the public degradation reason once its exact resource and field are fixed. This needs a stable API location, not log parsing.
		It("RHWA-1839-16A Target journal writes fail - A valid NHC request still causes reboot", reportxml.ID("RHWA-1839-16A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-16A")

			By("Complete the baseline with active NHC and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Block writes to the target journal directory. Confirm that a temporary file cannot be created.")
			journal := test.blockJournalWrites(0)
			Expect(journal.Confirm(test.ctx)).To(Succeed())

			By("Observe healthy storage with no CR, reboot, or restriction for 300 seconds.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)
			Expect(journal.Confirm(test.ctx)).To(Succeed())

			By("Reject target storage traffic while journal writes remain blocked. Require an NHC-owned CR.")
			fault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(fault)
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Require the target reboot, OutOfService, policy success, and removal of the original Pod.")
			test.waitForRequestedTargetBoot(request, requestCreatedAt, test.baseline)
			result := test.waitForOOSPolicySuccess(request, requestCreatedAt)
			test.waitForOriginalWorkloadEviction(result)
			test.assertPeerHostBootIDsUnchanged()
			test.assertNetworkFaultsPersistAfterReboot(fault)
			Expect(journal.Confirm(test.ctx)).To(Succeed())

			By("Restore journal writes and storage. Require NHC to delete the CR and check full worker recovery.")
			test.restoreJournalDirectoryFlags(journal)
			test.removeNetworkFault(fault)
			test.waitForHealthyStorage()
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, time.Now())
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-16C): Repeat with delayed host-filesystem writes. This needs a reversible host fault that does not affect the peer's API process.
		It("RHWA-1839-16C Peer journal writes fail - Peers still process the target request", reportxml.ID("RHWA-1839-16C"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-16C")

			By("Complete the baseline with active target-only NHC and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveTargetOnlyNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Block writes to every peer journal directory. Confirm the peer write failures.")
			peers := test.peerIndices()
			journalFaults := make([]*bpnOwnedJournalFault, 0, len(peers))
			for _, index := range peers {
				journalFaults = append(journalFaults, test.blockJournalWrites(index))
			}

			By("Reject target storage traffic with peer journal writes blocked. Require an NHC-owned CR.")
			targetFault := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(targetFault)
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Require the target reboot, OutOfService, policy success, and removal of the original Pod.")
			test.waitForRequestedTargetBoot(request, requestCreatedAt, test.baseline)
			result := test.waitForOOSPolicySuccess(request, requestCreatedAt)
			test.waitForOriginalWorkloadEviction(result)
			test.assertNetworkFaultsPersistAfterReboot(targetFault)

			By("Check that the peer journal faults persist and the peer agents run without reboots or restrictions.")
			for _, fault := range journalFaults {
				Expect(fault.Confirm(test.ctx)).To(Succeed())
			}
			test.assertPeerAgentsKeepRunningWithoutRestart(requestCreatedAt, time.Now())
			test.assertPeerHostBootIDsUnchanged()
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline, peers...)).
				To(Succeed(), "The peers must receive no CR or restriction")

			By("Restore peer journal writes and target storage. Require CR deletion and full worker recovery.")
			for _, fault := range journalFaults {
				test.restoreJournalDirectoryFlags(fault)
			}
			test.removeNetworkFault(targetFault)
			test.waitForHealthyStorage()
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, time.Now())
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-17B): Repeat with more requested CRs during the same unresolved outage. This needs additional reboot cycles but no internal state assertions.
		It("RHWA-1839-17B Continuous storage outage - Distinct requests reboot once and the autonomous limit persists", reportxml.ID("RHWA-1839-17B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17B")

			By("Complete autonomous preparation with active target-only NHC and the first Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveTargetOnlyNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.requireKnownHistoryAndObserveRecovery()

			By("Reject target storage traffic. Keep that fault active through both requests and API interruptions.")
			storage := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			api := test.prepareAndTrackNetworkFault(0, sbrutils.BlockAPILoss, true)
			test.applyAndConfirmNetworkFault(storage)
			test.waitForStorageUnhealthy(0)
			storageUnhealthyAt := time.Now()

			By("Require NHC request A, one target reboot, OutOfService, and removal of the first Pod.")
			requestA, requestACreatedAt := test.waitForNHCOwnedSBRCR()
			rebootA := test.waitForRequestedTargetBoot(requestA, requestACreatedAt, test.baseline)
			resultA := test.waitForOOSPolicySuccess(requestA, requestACreatedAt)
			test.waitForOriginalWorkloadEviction(resultA)
			test.assertNetworkFaultsPersistAfterReboot(storage)

			By("Keep request A active for 300 seconds without another reboot. Pause NHC and delete A.")
			test.observeBPNRequestWithoutRepeat(requestA, rebootA)
			test.setNHCRequestCreationPaused(true)
			test.deleteSBRAndWaitForBaselineRestrictions(requestA)
			test.waitForWorkerRecovery(corev1.ConditionTrue)
			releasedA := test.quietSince

			By("Block target API access. Require one autonomous reboot and no second reboot for 300 seconds.")
			test.applyAndConfirmNetworkFault(api)
			autonomousBoot, autonomousReboot := test.waitForTargetHostBootIDChange(api.Evidence().ActiveAt)
			test.nodes[0].boot = autonomousBoot
			test.assertNetworkFaultsPersistAfterReboot(storage, api)
			test.observeInterval(sbrparams.BPNNoReboot, releasedA, func() error {
				return test.checkBootsWithoutRequests(autonomousReboot, releasedA)
			})

			By("Restore only API access. Require worker recovery with target storage still unhealthy. Check A's retained identity if present.")
			test.removeNetworkFault(api)
			test.waitForWorkerRecovery(corev1.ConditionTrue)
			requestBHistoryFrom := time.Now()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.waitForCompleteAPIHistory(releasedA, time.Now(), false)
			for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
				if sample.At.Before(releasedA) || sample.Object.Annotations[bpnOwnerAnnotation] == "" {
					continue
				}
				owner, err := bpnParseRemediationOwnerAnnotation(sample.Object)
				Expect(err).NotTo(HaveOccurred())
				if owner.Execution.RequestUID != "" {
					Expect(owner.Execution.RequestUID).To(Equal(requestA.GetUID()),
						"A retained request identity must still identify request A before request B")
				}
			}

			By("Resume NHC. Require request B with a different UID, one target reboot, OutOfService, and removal of the second Pod.")
			test.setNHCRequestCreationPaused(false)
			requestB, requestBCreatedAt := test.waitForNHCOwnedSBRCR()
			Expect(requestB.GetUID()).NotTo(Equal(requestA.GetUID()), "NHC must create a distinct request B")
			rebootB := test.waitForRequestedTargetBoot(requestB, requestBCreatedAt, requestBHistoryFrom)
			resultB := test.waitForOOSPolicySuccess(requestB, requestBCreatedAt)
			test.waitForOriginalWorkloadEviction(resultB)
			test.assertNetworkFaultsPersistAfterReboot(storage)

			By("Keep request B active for 300 seconds without another reboot. Pause NHC and delete B.")
			test.observeBPNRequestWithoutRepeat(requestB, rebootB)
			test.setNHCRequestCreationPaused(true)
			test.deleteSBRAndWaitForBaselineRestrictions(requestB)
			test.waitForWorkerRecovery(corev1.ConditionTrue)

			By("Block API access again while storage is still unavailable. Observe no further reboot or request for 300 seconds.")
			test.applyAndConfirmNetworkFault(api)
			test.observeInterval(sbrparams.BPNNoReboot, test.quietSince, func() error {
				return test.checkBootsWithoutRequests(rebootB, test.quietSince)
			})
			test.assertNetworkFaultsPersistAfterReboot(storage, api)

			By("Restore storage and API access. Require full worker recovery.")
			faultEnd := time.Now()
			test.waitForCompleteAPIHistory(storageUnhealthyAt, faultEnd, false)
			Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionTrue, storageUnhealthyAt, faultEnd, 0)).
				To(Succeed(), "Target storage must remain unhealthy through both requests and API interruptions")
			test.removeTrackedNetworkFaults()
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		It("RHWA-1839-17E API loss interrupts recovery - Storage recovery still permits a later reboot", reportxml.ID("RHWA-1839-17E"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17E")

			By("Complete autonomous preparation and confirm all observers are ready.")
			test.prepareNodesAndCreateSBRConfig()
			test.createPausedNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.requireKnownHistoryAndObserveRecovery()

			By("Block storage, then API access. Observe the first changed host boot ID.")
			storage, api, from := test.applyStorageLossThenAPILoss()
			test.nodes[0].boot, _ = test.waitForTargetHostBootIDChange(from)
			test.assertNetworkFaultsPersistAfterReboot(storage, api)

			By("Keep the outage active for NO_REBOOT. Require no further observed reboot.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths. Start the first recovery window at the first restoration command's dispatch time.")
			recoveryStart := time.Now()
			test.removeNetworkFault(storage)
			test.removeNetworkFault(api)

			By("Confirm False storage health and live API transport. Record when both results become available.")
			confirmedFirst := test.waitForHealthyStorageAndAPIByDeadline(recoveryStart, recoveryStart.Add(sbrparams.BPNRecoveryPart))
			test.checkStorageHealthAndAPIAccessUntil(confirmedFirst, recoveryStart.Add(sbrparams.BPNRecoveryPart))

			By("Dispatch the API fault at 40s. Keep confirmed API loss active for RECOVERY_BREAK=20s while storage remains available.")
			test.applyAndConfirmNetworkFault(api)
			if time.Since(recoveryStart) >= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: the first possible healthy interval reached 60s")
			}
			firstEnd := api.Evidence().DispatchedAt
			test.waitUntil(api.Evidence().ActiveAt.Add(sbrparams.BPNRecoveryBreak))

			By("Restore API access. Start the second recovery window at that command's dispatch time. Confirm both healthy results again.")
			secondStart := time.Now()
			test.removeNetworkFault(api)
			confirmedSecond := test.waitForHealthyStorageAndAPIByDeadline(recoveryStart, secondStart.Add(sbrparams.BPNRecoveryPart))
			test.checkStorageHealthAndAPIAccessUntil(confirmedSecond, secondStart.Add(sbrparams.BPNRecoveryPart))

			By("Dispatch combined loss at the second window's 40s point. Require each possible healthy interval to remain below 60s.")
			secondEnd := time.Now()
			Expect(test.checkRecordedBootIDsUnchanged(recoveryStart)).To(Succeed(), "No host may reboot during storage recovery")
			test.applyAndConfirmNetworkFault(storage)
			test.applyAndConfirmNetworkFault(api)
			if time.Since(secondStart) >= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: the second possible healthy interval reached 60s")
			}

			By("Require the confirmed healthy intervals to total more than 60s. Apply the bounds in Section 4.5.")
			confirmedTotal := firstEnd.Sub(confirmedFirst) + secondEnd.Sub(confirmedSecond)
			if confirmedTotal <= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: confirmed healthy time did not exceed 60s in total")
			}
			test.waitForCompleteAPIHistory(confirmedFirst, secondEnd, true)
			Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, confirmedFirst, secondEnd, 0)).To(Succeed(), "Storage health must remain False across the API interruption")
			Expect(bpnPeerAgentInterval(test.podRecorder.Snapshot().Observations, test.name,
				test.nodes[0].initial.Name, confirmedFirst, secondEnd)).To(Succeed(), "The target agent must not restart during storage recovery")
			AddReportEntry("bpn-recovery-intervals", map[string]interface{}{
				"firstStart": recoveryStart, "firstConfirmed": confirmedFirst,
				"firstEnd": firstEnd, "secondStart": secondStart, "secondConfirmed": confirmedSecond,
				"secondEnd": secondEnd, "confirmedTotal": confirmedTotal.String(),
			})

			By("Require another changed host boot ID within REBOOT after the final combined outage.")
			test.nodes[0].boot, _ = test.waitForTargetHostBootIDChange(api.Evidence().ActiveAt)
			test.assertNetworkFaultsPersistAfterReboot(storage, api)

			By("Keep the final outage active for NO_REBOOT. Require no further observed reboot.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths and require full worker recovery.")
			test.removeTrackedNetworkFaults()
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		It("RHWA-1839-17F Storage loss interrupts recovery - No second autonomous reboot", reportxml.ID("RHWA-1839-17F"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17F")
			test.recoveryPeriod = sbrparams.BPNInterruptedRecoveryPeriod

			By("Configure 180s recovery. Complete autonomous preparation and confirm observer readiness.")
			test.prepareNodesAndCreateSBRConfig()
			test.createPausedNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.requireKnownHistoryAndObserveRecovery()

			By("Block storage, then API access. Observe the first changed host boot ID.")
			storage, api, from := test.applyStorageLossThenAPILoss()
			test.nodes[0].boot, _ = test.waitForTargetHostBootIDChange(from)
			test.assertNetworkFaultsPersistAfterReboot(storage, api)

			By("Keep the outage active for NO_REBOOT. Require no further observed reboot.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths. Confirm healthy storage and API access within the first 110s window.")
			firstStart := time.Now()
			test.removeNetworkFault(storage)
			test.removeNetworkFault(api)
			firstConfirmed := test.waitForHealthyStorageAndAPIByDeadline(firstStart, firstStart.Add(sbrparams.BPNInterruptedRecoveryPart))
			test.checkStorageHealthAndAPIAccessUntil(firstConfirmed, firstStart.Add(sbrparams.BPNInterruptedRecoveryPart))

			By("Interrupt storage at 110s. Require True storage health before the first window reaches 180s.")
			firstEnd := time.Now()
			test.applyAndConfirmNetworkFault(storage)
			firstUnhealthy, err := test.waitUnhealthy(0, nil)
			Expect(err).NotTo(HaveOccurred(), "The target must report storage failure while API access remains available")
			if !firstUnhealthy.Before(firstStart.Add(test.recoveryPeriod)) {
				Fail("INCONCLUSIVE: storage failure was not observed before the first recovery period ended")
			}

			By("Restore storage. Confirm healthy storage and API access within the second 110s window.")
			secondStart := time.Now()
			test.removeNetworkFault(storage)
			secondConfirmed := test.waitForHealthyStorageAndAPIByDeadline(secondStart, secondStart.Add(sbrparams.BPNInterruptedRecoveryPart))
			test.checkStorageHealthAndAPIAccessUntil(secondConfirmed, secondStart.Add(sbrparams.BPNInterruptedRecoveryPart))

			By("Interrupt storage at 110s. Require True storage health before the second window reaches 180s.")
			secondEnd := time.Now()
			test.applyAndConfirmNetworkFault(storage)
			secondUnhealthy, err := test.waitUnhealthy(0, nil)
			Expect(err).NotTo(HaveOccurred(), "The target must report the second storage failure")
			if !secondUnhealthy.Before(secondStart.Add(test.recoveryPeriod)) {
				Fail("INCONCLUSIVE: storage failure was not observed before the second recovery period ended")
			}

			By("Require more than 180s of combined healthy time. Check both interrupted intervals and target agent continuity.")
			confirmedTotal := firstEnd.Sub(firstConfirmed) + secondEnd.Sub(secondConfirmed)
			if confirmedTotal <= test.recoveryPeriod {
				Fail("INCONCLUSIVE: confirmed healthy time did not exceed 180s in total")
			}
			test.waitForCompleteAPIHistory(firstConfirmed, secondUnhealthy, true)
			Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, firstConfirmed, firstEnd, 0)).To(Succeed(), "First interval storage health")
			Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, secondConfirmed, secondEnd, 0)).To(Succeed(), "Second interval storage health")
			Expect(bpnPeerAgentInterval(test.podRecorder.Snapshot().Observations, test.name,
				test.nodes[0].initial.Name, firstConfirmed, secondUnhealthy)).To(Succeed(), "The target agent must not restart between recovery intervals")
			Expect(test.checkRecordedBootIDsUnchanged(firstStart)).To(Succeed(), "No host may reboot during interrupted storage recovery")
			AddReportEntry("bpn-interrupted-storage-recovery", map[string]interface{}{
				"firstStart": firstStart, "firstConfirmed": firstConfirmed, "firstEnd": firstEnd,
				"firstUnhealthy": firstUnhealthy, "secondStart": secondStart, "secondConfirmed": secondConfirmed,
				"secondEnd": secondEnd, "secondUnhealthy": secondUnhealthy, "confirmedTotal": confirmedTotal.String(),
			})

			By("Block API access while storage remains unhealthy. Require no reboot or request for NO_REBOOT.")
			test.applyAndConfirmNetworkFault(api)
			test.observeNoRebootWithoutRequests()

			By("Restore both paths and require full worker recovery.")
			test.removeTrackedNetworkFaults()
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})
		It("RHWA-1839-18 Combined outage with OnReboot - OutOfService waits for matching boot evidence", reportxml.ID("RHWA-1839-18"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-18")
			test.watcherGroupSize = 1
			test.oosTaintPolicy = sbrparams.BPNPolicyOnReboot

			By("Complete autonomous preparation with OnReboot, active NHC, and a Pod on the target.")
			test.prepareNodesAndCreateSBRConfig()
			Expect(bpnOutOfService(test.nodes[0].initial)).To(BeEmpty(), "The target must start without an OutOfService taint")
			test.createActiveNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.createNamespaceAndWaitForPinnedWorkloadPod()
			test.requireKnownHistoryAndObserveRecovery()
			originalBoot := test.nodes[0].boot

			By("Block target API access first. Then reject target storage traffic. Keep both faults across reboot.")
			api := test.prepareAndTrackNetworkFault(0, sbrutils.BlockAPILoss, true)
			storage := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(api)
			test.waitForHealthyStorage(0)
			test.assertCurrentHostBootIDsMatch(0)
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline)).To(Succeed())
			test.applyAndConfirmStorageLossDuringAPILoss(storage, api)
			outageAt := storage.Evidence().ActiveAt

			By("Require unhealthy target storage and the NHC-owned target CR.")
			test.waitForStorageUnhealthy(0)
			request, requestCreatedAt := test.waitForNHCOwnedSBRCR()

			By("Keep the target API blocked past the saved reboot deadline. Require no OutOfService or policy success.")
			test.waitPastRebootDeadlineWithoutOOS(request, requestCreatedAt, originalBoot)

			By("Confirm the autonomous reboot and both faults. Check that OutOfService and policy success remain absent.")
			observedBoot, _ := test.waitForTargetHostBootIDChange(outageAt)
			test.nodes[0].boot = observedBoot
			Expect(observedBoot).NotTo(Equal(originalBoot))
			test.assertNetworkFaultsPersistAfterReboot(storage, api)
			test.waitPastRebootDeadlineWithoutOOS(request, requestCreatedAt, originalBoot)

			By("Restore only target API access. Require matching boot evidence, OutOfService, and policy success.")
			apiRestoreAt := time.Now()
			test.removeNetworkFault(api)
			result := test.waitForOnRebootPolicySuccess(request, requestCreatedAt, apiRestoreAt, originalBoot, observedBoot)
			test.assertNetworkFaultsPersistAfterReboot(storage)

			By("Require removal of the original Pod while target storage remains unavailable.")
			test.waitForOriginalWorkloadEviction(result)
			test.assertPeerHostBootIDsUnchanged()
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline, test.peerIndices()...)).To(Succeed(),
				"Peer workers must retain their baseline restrictions and receive no SBR CR")

			By("Restore target storage. Require NHC to delete the CR, restriction release, and full worker recovery.")
			test.removeNetworkFault(storage)
			test.waitForHealthyStorage()
			test.waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(request, time.Now())
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})

		It("RHWA-1839-19 Runner creates remediation CRs - SBR makes no offer and the host does not reboot", reportxml.ID("RHWA-1839-19"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-19")
			test.nhcUnhealthyDuration = time.Hour

			By("Complete the baseline with active target-only NHC and a one-hour unhealthy duration.")
			test.prepareNodesAndCreateSBRConfig()
			test.createActiveTargetOnlyNHCAndSBRTemplate()
			test.waitForAgentReporterAndStorageReadiness()
			test.observeAndRecordHealthyBaseline(sbrparams.BPNBaseline)

			By("Reject target storage traffic with API access available. Require unhealthy storage and no NHC request.")
			storage := test.prepareAndTrackNetworkFault(0, sbrutils.BlockStorageLoss, true)
			test.applyAndConfirmNetworkFault(storage)
			unhealthyAt, err := test.waitUnhealthy(0, nil)
			Expect(err).NotTo(HaveOccurred(), "The target must report storage failure while API access remains available")
			earliestEligibleAt := storage.Evidence().DispatchedAt.Add(test.nhcUnhealthyDuration)
			test.waitForCompleteAPIHistory(test.baseline, time.Now(), false)
			Expect(test.checkNoSBRsOrRestrictionChanges(test.baseline)).To(Succeed())

			By("Attempt a runner-created CR without an owner. Observe no offer, reboot, cordon, or OutOfService for 300 seconds.")
			unowned, unownedFrom := test.createUnauthorizedTargetSBRCR(false)
			test.observeUnauthorizedRequestWithoutRemediation(unowned, unownedFrom)
			if unowned != nil {
				test.deleteSBRAndWaitForBaselineRestrictions(unowned)
			}

			By("Attempt a runner-created CR with an NHC owner reference. Repeat the negative observation for 300 seconds.")
			copied, copiedFrom := test.createUnauthorizedTargetSBRCR(true)
			test.observeUnauthorizedRequestWithoutRemediation(copied, copiedFrom)
			if copied != nil {
				test.deleteSBRAndWaitForBaselineRestrictions(copied)
			}

			By("Check that NHC remained below its unhealthy duration. Restore storage and require full worker recovery.")
			faultEnd := time.Now()
			Expect(faultEnd).To(BeTemporally("<", earliestEligibleAt),
				"Both runner observations must finish before NHC can create a request")
			test.waitForCompleteAPIHistory(unhealthyAt, faultEnd, false)
			Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionTrue, unhealthyAt, faultEnd, 0)).
				To(Succeed(), "Target storage must remain unhealthy during both runner observations")
			test.removeNetworkFault(storage)
			test.waitForWorkerRecovery(corev1.ConditionFalse)
		})
	})

func (test *bpnFixture) observeBPNRequestWithoutRepeat(request *unstructured.Unstructured, rebootObservedAt time.Time) {
	GinkgoHelper()
	test.observeInterval(sbrparams.BPNNoReboot, test.baseline, func() error {
		if err := test.checkRecordedBootIDsUnchanged(rebootObservedAt, 0); err != nil {
			return err
		}
		peers := test.peerIndices()
		if err := test.checkRecordedBootIDsUnchanged(test.baseline, peers...); err != nil {
			return err
		}
		if err := test.checkNoSBRsOrRestrictionChanges(test.baseline, peers...); err != nil {
			return err
		}

		return test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
			_, err := test.getAndCheckSBRCR(ctx, request)

			return err
		})
	})
}
