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
	Label(labels.OperatorSBR, labels.TierResiliency, labels.ComponentRemediation,
		labels.DisruptionDestructive, labels.PlatformAny, labels.FrequencyWeekly), func() {
		// TODO(RHWA-1839-01): Check an existing application during the interval. This needs an application-specific health endpoint and must not affect this test's result.
		It("RHWA-1839-01 Healthy storage and API — matrix row 1", reportxml.ID("RHWA-1839-01"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-01")

			By("Complete the baseline and observer-readiness procedure.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Record SBRStorageUnhealthy=False and the boot ID of each selected Node.")
			test.requireHealth(corev1.ConditionFalse)
			test.finalBoots()

			By("Observe all three Nodes for NO_REBOOT without injecting a fault.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Read each actual host boot ID again.")
			test.finalBoots()

			By("Delete the test resources through common cleanup.")
		})

		// TODO(RHWA-1839-02): Repeat during an actual API server outage. This needs an independent control-plane recovery procedure and independent API-event evidence.
		It("RHWA-1839-02 API unavailable to all selected SBR nodes — matrix row 2", reportxml.ID("RHWA-1839-02"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-02")

			By("Complete the baseline and confirm that all observers are ready.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Prepare API-loss rules for all three selected Nodes.")
			faults := make([]*sbrutils.BlockNetworkFault, 0, 3)
			for index := range test.nodes {
				faults = append(faults, test.prepareFault(index, sbrutils.BlockAPILoss, false))
			}

			By("Activate those rules. Confirm API transport failure from each affected namespace.")
			for _, fault := range faults {
				test.activate(fault)
			}

			By("Start the 300s interval after the last Node confirms the fault.")
			By("Sample actual host boot IDs throughout the interval.")
			test.observeNoRebootWithoutRequests()

			By("Remove all API rules. Observe restored access and healthy storage conditions.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)

			By("Continue boot observations for POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-03A): Count every target boot before SSH returns. This needs a trusted host boot counter and does not affect the initial result.
		It("RHWA-1839-03A NHC limits requested remediation during broad storage failure — matrix row 3", reportxml.ID("RHWA-1839-03A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-03A")

			By("Complete the baseline with active NHC and minHealthy=2.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Prepare storage faults on all three workers. Make the target fault persistent across reboot.")
			faults := make([]*sbrutils.BlockNetworkFault, 0, 3)
			for index := range test.nodes {
				faults = append(faults, test.prepareFault(index, sbrutils.BlockStorageLoss, index == 0))
			}
			peersRestored := false
			restorePeers := func() {
				if peersRestored {
					return
				}
				test.restoreFault(faults[1])
				test.restoreFault(faults[2])
				test.requireHealth(corev1.ConditionFalse, 1, 2)
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
			test.activate(faults[0])
			test.requireUnhealthy(0)

			By("Observe A's NHC-created SBR CR within REQUEST_CREATE. Record its UID before disrupting either peer.")
			request, from := test.waitNHCRequest()
			checkPeers := func() error {
				if err := test.checkBoots(test.baseline, 1, 2); err != nil {
					return err
				}

				return test.noRestrictions(test.baseline, 1, 2)
			}
			Expect(checkPeers()).To(Succeed(), "Neither peer may receive a request or reboot")

			By("Reject storage traffic on workers B and C. Preserve API and SSH access on all three workers.")
			test.activate(faults[1])
			test.activate(faults[2])
			outageDeadline, err := bpnStorageObservationDeadline(test.healthFaults, 1, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Require True storage conditions on B and C within HEALTH_CHANGE. Check that neither peer receives a request or reboots.")
			test.requireUnhealthyWhile(checkPeers, 1, 2)

			By("Require A to reboot and its CR to satisfy the common requested-remediation result.")
			requestBoot, rebootObservedAt := test.waitReboot(from)
			test.nodes[0].boot = requestBoot
			test.requireRequestedResult(request, from, rebootObservedAt)

			By("Confirm that all storage faults remain active after A reboots.")
			test.checkFaultAfterReboot(faults...)

			By("Keep all faults active until the requested result and NO_REBOOT after the last peer fault are complete.")
			check := func() error {
				if err := checkPeers(); err != nil {
					return err
				}

				return test.checkBoots(rebootObservedAt, 0)
			}
			if remaining := time.Until(outageDeadline); remaining > 0 {
				Consistently(check, remaining, sbrparams.BPNPoll).Should(Succeed())
			}
			test.finalBoots()
			test.coverage(test.baseline, time.Now(), false)
			Expect(check()).To(Succeed(), "Only A may reboot. Neither peer may receive a request or restriction")

			By("Restore storage on B and C. Require both storage conditions to be False before restoring A.")
			restorePeers()

			By("Restore storage on A. Require False storage conditions on all three workers.")
			test.restoreFault(faults[0])
			test.requireHealth(corev1.ConditionFalse)
			healthyAt := time.Now()

			By("Let NHC delete A's CR after its 60s healthy delay. Require SBR restriction release.")
			test.waitNHCRelease(request, healthyAt)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			test.coverage(test.baseline, time.Now(), false)
			Expect(check()).To(Succeed(), "Peer requests and further reboots must remain absent through restoration")
		})

		// TODO(RHWA-1839-03B): Remove one peer during the request and check completion through the remaining peer. This needs a separate controlled peer-stop fixture.
		It("RHWA-1839-03B Requested reboot while every selected node has storage failure — matrix row 3", reportxml.ID("RHWA-1839-03B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-03B")

			By("Complete the baseline and start all required observers.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Reject storage traffic on all three workers. Use the persistent fault handle on the target.")
			faults := make([]*sbrutils.BlockNetworkFault, 0, 3)
			for _, index := range []int{0, 1, 2} {
				faults = append(faults, test.prepareFault(index, sbrutils.BlockStorageLoss, index == 0))
			}
			for _, fault := range faults {
				test.activate(fault)
			}

			By("Require True storage conditions on all three Nodes within HEALTH_CHANGE.")
			test.requireUnhealthy(0, 1, 2)

			By("Create one SBR CR for the target from the runner.")
			from := time.Now()
			request, requestDispatchedAt := test.request()

			By("Keep both peer storage faults active while observing the target reboot and CR result.")
			By("Require the common requested-remediation result.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot
			for _, fault := range faults {
				test.checkFaultAfterReboot(fault)
			}
			test.peerContinuity(from, time.Now())
			Expect(test.healthHistory(corev1.ConditionTrue, from, time.Now(), 1, 2)).To(Succeed(), "Both peer Nodes must remain storage-unhealthy through request completion")
			test.requireHealth(corev1.ConditionTrue, 1, 2)
			test.finalBoots(1, 2)

			By("Restore all storage access. Normally delete the CR after the target reports False.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
			test.releaseRequest(request)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-04A): Count every boot during the initial SSH blackout. This needs a trusted host boot counter and does not change this test's pass result.
		It("RHWA-1839-04A Combined storage and API outage — matrix row 4", reportxml.ID("RHWA-1839-04A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-04A")

			By("Complete the baseline, autonomous preparation, and observer-readiness procedure.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.autonomousPreparation()

			By("Prepare persistent storage-loss and API-loss rules on the target.")
			storage := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
			api := test.prepareFault(0, sbrutils.BlockAPILoss, true)

			By("Establish the first combined outage in the order specified in Section 4.5.")
			test.activate(storage)
			test.requireUnhealthy(0)
			test.finalBoots(0)
			test.activate(api)
			from := time.Now()

			By("Observe an actual changed host boot ID within REBOOT.")
			test.nodes[0].boot, _ = test.waitReboot(from)

			By("Confirm that both faults still apply after reconnection.")
			test.checkFaultAfterReboot(storage, api)

			By("Observe the new boot ID for NO_REBOOT while the outage continues.")
			test.observeNoRebootWithoutRequests()

			By("Restore storage and API access. Require False storage health and matching API/host boot IDs.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
			test.readyNodes()

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			test.coverage(from, time.Now(), false)
			Expect(test.noRestrictions(from)).To(Succeed())
		})

		// TODO(RHWA-1839-04B): Repeat with a still-active accepted request. This needs a repeatable external timing window and must not become an internal execution-path test.
		It("RHWA-1839-04B Autonomous reboot preserves a previous request identity — matrix row 4", reportxml.ID("RHWA-1839-04B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-04B")

			By("Complete the baseline and start observers before the preparatory request.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Create one SBR CR. Require the common requested-remediation result.")
			request, requestDispatchedAt := test.request()
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Normally delete that CR. Require release of its restrictions.")
			test.releaseRequest(request)

			By("Record its retained execution.requestUID from sbr.medik8s.io/remediation-owner.")
			retained := test.retainedRequest()
			Expect(retained).To(Equal(request.GetUID()))

			By("Record the current host boot ID. Complete autonomous preparation.")
			test.autonomousPreparation()

			By("Establish the first combined outage through Section 4.5. Observe a changed host boot ID.")
			storage := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
			api := test.prepareFault(0, sbrutils.BlockAPILoss, true)
			test.activate(storage)
			test.requireUnhealthy(0)
			test.finalBoots(0)
			test.activate(api)
			from := time.Now()
			test.nodes[0].boot, _ = test.waitReboot(from)
			test.checkFaultAfterReboot(storage, api)

			By("Keep the outage active for NO_REBOOT after that changed ID.")
			test.observeNoRebootWithoutRequests()

			By("Restore access. Compare the retained request UID with the recorded value.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
			test.readyNodes()
			Expect(test.retainedRequest()).To(Equal(retained))

			By("Complete common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
			test.coverage(from, time.Now(), false)
			Expect(test.noRestrictions(from)).To(Succeed())
		})

		// TODO(RHWA-1839-05A): Repeat with API transport failures through an additional endpoint topology. This needs that topology and no new required assertion.
		It("RHWA-1839-05A One node loses API access — matrix row 5", reportxml.ID("RHWA-1839-05A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-05A")

			By("Complete the baseline and confirm observer readiness.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Reject the target's API traffic. Confirm the transport fault.")
			fault := test.prepareFault(0, sbrutils.BlockAPILoss, false)
			test.activate(fault)

			By("Observe the target and peers for NO_REBOOT.")
			test.observeNoRebootWithoutRequests()

			By("Remove the API fault. Require restored transport and False storage health.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)

			By("Observe POST_RESTORE. Complete common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-05B): Observe a VM during the same partition. This needs an existing VM fixture and an independent workload observer.
		It("RHWA-1839-05B One node loses cluster communication — matrix row 5", reportxml.ID("RHWA-1839-05B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-05B")

			By("Complete the baseline and confirm all observers are ready.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Prepare the singleton-isolation rules with explicit storage and SSH exceptions.")
			fault := test.prepareFault(0, sbrutils.BlockSingletonIsolation, false)

			By("Activate the rules. Confirm blocked API and peer transport from worker A.")
			test.activate(fault)

			By("Observe actual host boot IDs and target CR events for NO_REBOOT.")
			test.observeNoRebootWithoutRequests()

			By("Remove the isolation rules. Require restored connections and False storage health.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-06): Check VMI removal or CSI attachment removal. These need suitable existing workload fixtures and are independent of this initial Pod result.
		It("RHWA-1839-06 NHC remediates one storage-unhealthy node — matrix row 6", reportxml.ID("RHWA-1839-06"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-06")

			By("Complete the baseline. Start CR, Node, host, and workload observers before the fault.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.createWorkload()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Record the workload Pod UID and all host boot IDs.")
			Expect(test.workload.UID).NotTo(BeEmpty())
			test.finalBoots()

			By("Reject storage traffic only on the target. Confirm that the fault will persist across reboot.")
			fault := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
			test.activate(fault)

			By("Require SBRStorageUnhealthy=True within HEALTH_CHANGE.")
			test.requireUnhealthy(0)

			By("Observe the NHC-created SBR CR within REQUEST_CREATE. Record its UID.")
			request, from := test.waitNHCRequest()

			By("Require the target to reboot within REBOOT.")
			requestBoot, requestRebootObservedAt := test.waitReboot(from)
			test.nodes[0].boot = requestBoot

			By("Confirm that the target storage fault remains active after reboot.")
			test.checkFaultAfterReboot(fault)

			By("Require the common requested-remediation result.")
			test.requireRequestedResult(request, from, requestRebootObservedAt)
			test.requirePeerBoots()

			By("Require the original workload Pod UID to disappear within WORKLOAD_GONE after OutOfService is observed.")
			var taintObserved time.Time
			for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
				if !sample.At.Before(from) && bpnOutOfService(sample.Object) == "nodeshutdown:NoExecute" {
					taintObserved = sample.At

					break
				}
			}
			deadline := taintObserved.Add(sbrparams.BPNWorkloadGone)
			remaining := time.Until(deadline)
			checkGone := func() bool {
				return bpnPodGoneBefore(test.podRecorder.Snapshot().Observations, test.workload, taintObserved, deadline)
			}
			if remaining > 0 {
				Eventually(checkGone, remaining, sbrparams.BPNPoll).Should(BeTrue(), "Remove the Pod within WORKLOAD_GONE")
			}
			Expect(checkGone()).To(BeTrue(), "UID-scoped Pod evidence must prove removal before WORKLOAD_GONE")

			By("Restore target storage. Require False storage health.")
			test.restoreFault(fault)
			test.requireHealth(corev1.ConditionFalse)
			healthyAt := time.Now()

			By("Let NHC delete the CR after its 60s healthy delay. Require SBR restriction release.")
			test.waitNHCRelease(request, healthyAt)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-07): Use a true kernel-freeze stimulus. This needs a supported trigger and recovery method already available on the target platform.
		It("RHWA-1839-07 Existing SysRq crash stimulus — matrix row 7", reportxml.ID("RHWA-1839-07"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-07")

			By("Complete the baseline and confirm Node and host observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Save /proc/sys/kernel/sysrq and /proc/sys/kernel/panic through SSH.")
			settings := &bpnHostSettings{host: test.nodes[0].host}
			test.hostSettings = settings
			settings.sysctlSaved = test.runHost("cat /proc/sys/kernel/sysrq /proc/sys/kernel/panic")
			Expect(strings.Fields(settings.sysctlSaved)).To(HaveLen(2))
			_, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo test -w /proc/sysrq-trigger")
			test.prerequisite(err, "existing SysRq trigger")

			By("Set SysRq to 1. Run sync. Send SysRq s.")
			test.disrupted = true
			test.runHost("sudo sh -c 'echo 1 > /proc/sys/kernel/sysrq; sync; echo s > /proc/sysrq-trigger'")

			By("Wait SYSRQ_SETTLE. Set kernel.panic to SYSRQ_PANIC_DELAY.")
			test.waitUntil(time.Now().Add(sbrparams.BPNSysRqSettle))
			test.runHost("sudo sysctl -w kernel.panic=180")

			By("Send c to /proc/sysrq-trigger through the prepared SSH connection.")
			test.finalBoots(0)
			from := time.Now()
			// SysRq injects a kernel panic, not a general kernel freeze.
			// A changed boot ID proves a reboot, not its mechanism.
			_, triggerErr := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, "sudo sh -c 'echo c > /proc/sysrq-trigger'")
			AddReportEntry("sysrq-dispatch", fmt.Sprint(triggerErr))

			By("Observe a changed actual host boot ID within REBOOT.")
			test.nodes[0].boot, _ = test.waitReboot(from)

			By("Restore the saved settings after the target returns.")
			values := strings.Fields(settings.sysctlSaved)
			test.runHost("sudo sysctl -w kernel.sysrq=" + bpnQuote(values[0]) + " kernel.panic=" + bpnQuote(values[1]))

			By("Require Ready and False storage health. Complete common cleanup.")
			test.readyNodes()
			test.requireHealth(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-08): Retain a completed remediation CR during this restart and check for no repeated host reboot. This needs one preparatory remediation cycle.
		It("RHWA-1839-08 Planned SBR agent Pod restart — matrix row 8", reportxml.ID("RHWA-1839-08"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-08")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Record the target agent Pod UID and all host boot IDs.")
			var original *corev1.Pod
			Expect(test.api(func(ctx context.Context) error {
				var err error
				original, err = test.targetAgent(ctx)

				return err
			})).To(Succeed())
			test.finalBoots()

			By("Delete only the target agent Pod through normal Kubernetes deletion.")
			test.deleteAgent(original)

			By("Require a different agent Pod UID on the same target Node within AGENT_READY.")
			test.waitAgentReplacement(original)

			By("Require the replacement agent to become Ready and the target storage condition to be False.")
			test.requireHealth(corev1.ConditionFalse)

			By("Continue unchanged-host-boot observation for NO_REBOOT after the replacement becomes Ready.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-09): Check an existing application during the stopped-kubelet interval. This needs a workload health check independent of kubelet status.
		It("RHWA-1839-09 Kubelet stops while storage remains healthy — matrix row 9", reportxml.ID("RHWA-1839-09"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-09")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

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
			test.readyNodes()
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-10A): Check a supported sub-timeout latency profile without unhealthy storage. This needs a separately agreed complete-probe latency bound.
		It("RHWA-1839-10A Slow storage exceeds the configured probe deadline — matrix row 10", reportxml.ID("RHWA-1839-10A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-10A")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Prepare the owned storage traffic class with a 20s delay.")
			fault := test.prepareFault(0, sbrutils.BlockStorageLatency, false)

			By("Activate the class. Confirm that storage traffic uses it.")
			test.activate(fault)

			By("Require SBRStorageUnhealthy=True within HEALTH_CHANGE.")
			By("Keep the latency active for NO_REBOOT after activation.")
			test.observeStorageFaults(0)

			By("Remove the owned traffic-control objects. Require False storage health.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-10B): Repeat with a qualified device-level I/O hang. This needs a reversible driver-specific fault already available on the cluster.
		It("RHWA-1839-10B Storage traffic remains unanswered — matrix row 10", reportxml.ID("RHWA-1839-10B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-10B")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Activate target storage DROP rules. Confirm matching traffic.")
			fault := test.prepareFault(0, sbrutils.BlockStorageTimeout, false)
			test.activate(fault)

			By("Require SBRStorageUnhealthy=True within HEALTH_CHANGE.")
			By("Keep the DROP rules active for NO_REBOOT after activation.")
			test.observeStorageFaults(0)

			By("Remove the DROP rules. Require False storage health.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-10C): Restart one peer while the other remains storage-unhealthy. This needs a separate controlled peer-restart case.
		It("RHWA-1839-10C Peers process a request while their storage times out — matrix row 10", reportxml.ID("RHWA-1839-10C"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-10C")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Drop storage traffic on both peers. Confirm both faults.")
			faults := make([]*sbrutils.BlockNetworkFault, 0, 2)
			for _, index := range []int{1, 2} {
				faults = append(faults, test.prepareFault(index, sbrutils.BlockStorageTimeout, false))
			}
			for _, fault := range faults {
				test.activate(fault)
			}

			By("Require True storage conditions on both peers within HEALTH_CHANGE.")
			test.requireUnhealthy(1, 2)

			By("Create a runner-owned SBR CR for the target.")
			from := time.Now()
			request, requestDispatchedAt := test.request()

			By("Require the common requested-remediation result while both peer faults remain active.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Confirm that neither peer changed its host boot ID.")
			for _, fault := range faults {
				test.checkFaultAfterReboot(fault)
			}
			test.peerContinuity(from, time.Now())
			Expect(test.healthHistory(corev1.ConditionTrue, from, time.Now(), 1, 2)).To(Succeed(), "Both peer Nodes must remain storage-unhealthy through request completion")
			test.requireHealth(corev1.ConditionTrue, 1, 2)
			test.finalBoots(1, 2)

			By("Restore peer storage. Normally delete the target CR.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
			test.releaseRequest(request)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-11): Check a pre-existing application's availability during the partition. This needs an application-specific observer and does not establish duplicate-writer protection.
		It("RHWA-1839-11 Group partition with healthy storage — matrix row 11", reportxml.ID("RHWA-1839-11"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-11")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createActivePolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Prepare rules for both directions of the worker-group partition.")
			faults := []*sbrutils.BlockNetworkFault{test.prepareFault(0, sbrutils.BlockGroupPartition, false)}
			// B and C preserve their mutual traffic. Only their links to A are blocked.
			for _, index := range []int{1, 2} {
				network := test.networks[index]
				network.Peers = nil
				for _, address := range test.nodes[0].initial.Status.Addresses {
					if address.Type == corev1.NodeInternalIP || address.Type == corev1.NodeExternalIP {
						network.Peers = append(network.Peers, address.Address)
					}
				}
				test.networks[index] = network
				faults = append(faults, test.prepareFault(index, sbrutils.BlockGroupPartition, false))
			}

			By("Activate all rules. Confirm blocked cross-group transport and retained API transport.")
			for _, fault := range faults {
				test.activate(fault)
			}

			By("Observe all selected Nodes for NO_REBOOT after the last rule becomes active.")
			test.observeHealthyWithoutRequests(sbrparams.BPNNoReboot)

			By("Remove all partition rules. Confirm cross-group transport returns.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)

			By("Observe POST_RESTORE before common cleanup.")
			test.observeHealthyWithoutRequests(sbrparams.BPNPostRestore)
		})

		// TODO(RHWA-1839-12A): Check fresh storage success before the requested reboot. This needs a sufficiently long externally observable acceptance window and adds no initial pass condition.
		It("RHWA-1839-12A Storage recovers after request acceptance — matrix row 12", reportxml.ID("RHWA-1839-12A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-12A")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Reject target storage traffic. Require True storage health.")
			fault := test.prepareFault(0, sbrutils.BlockStorageLoss, false)
			test.activate(fault)
			test.requireUnhealthy(0)

			By("Record the original host boot ID. Create a target SBR CR.")
			request, requestDispatchedAt := test.request()

			By("Wait for a public Accepted record matching this CR UID and Node UID.")
			accepted := test.acceptance(request, requestDispatchedAt)

			By("Immediately remove the storage fault through prepared SSH.")
			test.restoreFault(fault)

			By("Record the removal acknowledgement. Immediately read the actual host boot ID.")
			Expect(fault.Evidence().RestoreDispatchedAt.Before(accepted)).To(BeFalse(), "Acceptance must precede recovery")
			current, err := test.nodes[0].host.BootID(test.ctx, sbrparams.BPNHostCall)

			By("Require that this read still equals the original boot ID. Otherwise, fail the test for a timing miss.")
			Expect(err).NotTo(HaveOccurred(), "Row 12 requires a host read after recovery and before reboot")
			Expect(current).To(Equal(test.nodes[0].boot), "Row 12 requires the original host boot after recovery")
			Expect(test.nodes[0].recorder.Coverage(requestDispatchedAt, time.Now())).To(Succeed(), "Row 12 requires complete event order")

			By("Observe the later changed boot ID and require the common requested-remediation result.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Normally delete the CR and complete common cleanup.")
			test.releaseRequest(request)
		})

		// TODO(RHWA-1839-12B): Repeat with a longer post-acceptance API interruption. This needs a repeatable external timing window without modifying SBR internals.
		It("RHWA-1839-12B API recovers after request acceptance — matrix row 12", reportxml.ID("RHWA-1839-12B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-12B")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Prepare the target API fault without activating it.")
			fault := test.prepareFault(0, sbrutils.BlockAPILoss, false)

			By("Record the original host boot ID. Create a target SBR CR.")
			request, requestDispatchedAt := test.request()

			By("Observe the matching public Accepted record.")
			accepted := test.acceptance(request, requestDispatchedAt)

			By("Activate API-loss rules immediately. Confirm transport failure from the affected namespace.")
			test.activateFault(fault, "FAIL: Row 12 timing or ordering failure: ")

			By("Remove the API rules immediately. Confirm restored transport.")
			test.restoreFault(fault)
			Expect(fault.Evidence().RestoreDispatchedAt.Before(accepted)).To(BeFalse(), "Acceptance must precede recovery")

			By("Read the actual host boot ID. Require the original value to remain present.")
			current, err := test.nodes[0].host.BootID(test.ctx, sbrparams.BPNHostCall)
			Expect(err).NotTo(HaveOccurred(), "Row 12 requires a host read after recovery and before reboot")
			Expect(current).To(Equal(test.nodes[0].boot), "Row 12 requires the original host boot after recovery")
			Expect(test.nodes[0].recorder.Coverage(requestDispatchedAt, time.Now())).To(Succeed(), "Row 12 requires complete event order")

			By("Observe the subsequent reboot and common requested-remediation result.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Normally delete the CR and complete common cleanup.")
			test.releaseRequest(request)
		})

		// TODO(RHWA-1839-15A): Retain a VMI or attachment instead of a Pod. This needs an existing suitable fixture and a reversible retention method.
		It("RHWA-1839-15A A terminating Pod does not retain the remediation CR — matrix row 15", reportxml.ID("RHWA-1839-15A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-15A")

			By("Complete the baseline and start all Node, CR, host, and Pod observers.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.createWorkload()
			test.holdWorkloadWithFinalizer()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Record the test Pod UID. Request normal Pod deletion without removing its finalizer.")
			test.retainPod()

			By("Require a deletion timestamp and the unchanged test finalizer within FAULT_READY.")
			Expect(test.api(test.heldPod)).To(Succeed())

			By("Create a target SBR CR and observe the host reboot.")
			request, requestDispatchedAt := test.request()

			By("Observe OutOfService and the CR's fixed cleanup start and deadline.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			evidence := test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			Expect(bpnRetainedCleanupHistory(test.crRecorder.Snapshot().Observations, request, evidence.deadline)).To(Succeed())
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Keep the finalizer present through the stored cleanup deadline.")
			var deadline time.Time
			Expect(test.api(func(ctx context.Context) error {
				current, err := test.requestCurrent(ctx, request)
				if err != nil {
					return err
				}
				_, deadline, err = bpnCleanupTimes(current)

				return err
			})).To(Succeed())
			test.waitUntil(deadline)
			Expect(test.api(test.heldPod)).To(Succeed(), "The same finalizer-held Pod must remain at cleanup expiry")

			By("Require terminal CR conditions by the deadline plus PUBLISH.")
			Expect(test.api(func(ctx context.Context) error {
				current, err := test.requestCurrent(ctx, request)
				if err != nil {
					return err
				}
				phase, _, _ := unstructured.NestedString(current.Object, "status", "cleanup", "phase")
				if phase != "Incomplete" && phase != "Unknown" {
					return fmt.Errorf("retained Pod requires Incomplete or Unknown cleanup, got %q", phase)
				}
				for _, sample := range test.crRecorder.Snapshot().Observations {
					if sample.UID != request.GetUID() {
						continue
					}
					observed, _, _ := unstructured.NestedString(sample.Object.Object, "status", "cleanup", "phase")
					if observed == "Completed" {
						return fmt.Errorf("SBR reported Completed while the original Pod remained")
					}
				}

				return nil
			})).To(Succeed())

			By("Normally delete the CR while the same terminating Pod still exists.")
			test.releaseRequest(request)

			By("Require the CR to disappear and SBR's restrictions to disappear within RELEASE.")
			Expect(test.api(test.heldPod)).To(Succeed(), "The same held Pod must remain during normal CR deletion")
			test.requireRestrictionsReleased()

			By("Remove the test-owned Pod finalizer. Complete common cleanup.")
		})

		// TODO(RHWA-1839-15B): Keep two completed CRs on the same Node and delete them separately. This needs two request cycles and checks retention of shared restrictions.
		It("RHWA-1839-15B Preserve administrator-owned restrictions during CR deletion — matrix row 15", reportxml.ID("RHWA-1839-15B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-15B")

			By("Start observers and complete the common healthy preparation before adding the administrator fixtures.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.createWorkload()
			test.holdWorkloadWithFinalizer()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Make the test Pod terminating with its finalizer retained, as in case 15A.")
			test.retainPod()

			By("Add the three administrator fixtures. Record their exact values before creating the CR.")
			Expect(test.setAdministratorFixtures(true)).To(Succeed())

			By("Confirm that agents and reporters remain available. Record the target boot baseline again.")
			test.eventually(sbrparams.BPNAgentReady, "Agents must tolerate existing OutOfService", test.componentsReady)
			test.finalBoots()

			By("Create a target SBR CR. Observe its reboot and fixed cleanup deadline.")
			request, requestDispatchedAt := test.request()
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			evidence := test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			Expect(bpnRetainedCleanupHistory(test.crRecorder.Snapshot().Observations, request, evidence.deadline)).To(Succeed())
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Keep the Pod retained through expiry. Require terminal conditions with Incomplete or Unknown cleanup.")
			var deadline time.Time
			Expect(test.api(func(ctx context.Context) error {
				current, err := test.requestCurrent(ctx, request)
				if err != nil {
					return err
				}
				_, deadline, err = bpnCleanupTimes(current)

				return err
			})).To(Succeed())
			test.waitUntil(deadline)
			Expect(test.api(test.heldPod)).To(Succeed(), "The same finalizer-held Pod must remain at cleanup expiry")
			Expect(test.api(func(ctx context.Context) error {
				current, err := test.requestCurrent(ctx, request)
				if err != nil {
					return err
				}
				phase, _, _ := unstructured.NestedString(current.Object, "status", "cleanup", "phase")
				if phase != "Incomplete" && phase != "Unknown" {
					return fmt.Errorf("retained Pod requires Incomplete or Unknown cleanup, got %q", phase)
				}
				for _, sample := range test.crRecorder.Snapshot().Observations {
					if sample.UID != request.GetUID() {
						continue
					}
					observed, _, _ := unstructured.NestedString(sample.Object.Object, "status", "cleanup", "phase")
					if observed == "Completed" {
						return fmt.Errorf("SBR reported Completed while the original Pod remained")
					}
				}

				return nil
			})).To(Succeed())

			By("Normally delete the CR while the Pod and administrator fixtures still exist.")
			test.releaseRequest(request)

			By("Require CR removal within RELEASE. Check that all three administrator fixtures remain unchanged.")
			Expect(test.api(test.heldPod)).To(Succeed(), "The same held Pod must remain during normal CR deletion")
			test.requireRestrictionsReleased()

			By("Remove only the administrator fixtures added by this case. Remove the test Pod finalizer.")
			Expect(test.setAdministratorFixtures(false)).To(Succeed())

			By("Complete common cleanup against the original Node baseline.")
		})

		// TODO(RHWA-1839-16A): Check the public degradation reason once its exact resource and field are fixed. This needs a stable API location, not log parsing.
		It("RHWA-1839-16A Journal writes unavailable before a new requested reboot — matrix row 16", reportxml.ID("RHWA-1839-16A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-16A")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Save the state directory's existing flags. Add only its immutable flag.")
			journal := test.journal(0)
			journal.releaseRequestsBeforeRestore = true

			By("Confirm that a test-owned temporary file cannot be created in that directory.")
			Expect(journal.Confirm(test.ctx)).To(Succeed())

			By("Create a runner-owned SBR CR for the target.")
			request, requestDispatchedAt := test.request()

			By("Keep the directory unavailable for NO_REBOOT after CR creation.")
			test.observeHealthyBoots(sbrparams.BPNNoReboot)

			By("Require the unchanged host boot ID and no success condition for that CR throughout the interval.")
			test.finalBoots()
			test.coverage(requestDispatchedAt, time.Now(), false)
			for _, sample := range test.crRecorder.Snapshot().Observations {
				if sample.UID == request.GetUID() {
					succeeded := bpnCondition(sample.Object, "FencingSucceeded", "True") || bpnCondition(sample.Object, "Succeeded", "True")
					Expect(succeeded).To(BeFalse(), "An unexecuted request must never report success")
				}
			}

			By("Request normal CR deletion while the directory remains unavailable.")
			test.releaseRequest(request)

			By("Require CR removal and release of any SBR restrictions within RELEASE.")
			By("Restore the directory flag. Check temporary file creation and removal.")
			test.restoreJournal(journal)

			By("Complete common cleanup.")
		})

		// TODO(RHWA-1839-16B): Check automatic resumption without an agent restart. This needs a specified public recovery contract and must not affect this initial result.
		It("RHWA-1839-16B Restore journal access and resume requested remediation — matrix row 16", reportxml.ID("RHWA-1839-16B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-16B")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Apply the immutable-directory fault. Confirm failed temporary file creation.")
			journal := test.journal(0)

			By("Restore the original directory flags. Confirm successful temporary file creation and removal.")
			test.restoreJournal(journal)

			By("Normally delete the target agent Pod once so its replacement starts with the restored directory.")
			test.restartAgent()

			By("Require a new Ready agent Pod, False storage health, and the unchanged host boot ID.")
			test.finalBoots()
			test.requireHealth(corev1.ConditionFalse)

			By("Create a new target SBR CR after that restart completes.")
			request, requestDispatchedAt := test.request()

			By("Require the common requested-remediation result.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Normally delete the CR. Complete common cleanup.")
			test.releaseRequest(request)
		})

		// TODO(RHWA-1839-16C): Repeat with delayed host-filesystem writes. This needs a reversible host fault that does not affect the peer's API process.
		It("RHWA-1839-16C Peers reconcile while their journal directories reject writes — matrix row 16", reportxml.ID("RHWA-1839-16C"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-16C")

			By("Complete the baseline and confirm all observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Apply the immutable-directory fault to each peer. Confirm both access failures.")
			first, second := test.journal(1), test.journal(2)

			By("Create a valid SBR CR for the unaffected target.")
			from := time.Now()
			request, requestDispatchedAt := test.request()

			By("Keep both peer faults active while observing the requested result.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot

			By("Require the common requested-remediation result and unchanged peer host boot IDs.")
			for _, fault := range []*bpnOwnedJournalFault{first, second} {
				Expect(fault.Confirm(test.ctx)).To(Succeed())
			}
			test.peerContinuity(from, time.Now())
			test.finalBoots(1, 2)

			By("Restore both peer directory flags. Check temporary file creation and removal.")
			test.restoreJournal(first)
			test.restoreJournal(second)

			By("Normally delete the target CR. Complete common cleanup.")
			test.releaseRequest(request)
		})

		// TODO(RHWA-1839-17A): Count every boot during each initial blackout. This needs a trusted boot counter. Two observed request cycles do not prove an unlimited number of cycles.
		It("RHWA-1839-17A Successive valid requests during persistent storage failure — matrix row 17", reportxml.ID("RHWA-1839-17A"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17A")

			By("Complete the baseline and confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.establishBaseline(sbrparams.BPNBaseline)

			By("Apply persistent target storage loss. Require True storage health.")
			storage := test.prepareFault(0, sbrutils.BlockStorageLoss, true)
			test.activate(storage)
			test.requireUnhealthy(0)
			test.requireAgentRunning(time.Time{})

			By("Create CR A. Record its UID and require the common requested-remediation result.")
			first, firstDispatchedAt := test.request()
			firstBoot, firstRebootObservedAt := test.waitReboot(firstDispatchedAt)
			test.requireAgentRunning(firstRebootObservedAt)
			test.requireRequestedResult(first, firstDispatchedAt, firstRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = firstBoot
			test.checkFaultAfterReboot(storage)

			By("Retain CR A for NO_REBOOT after its result. Require the new host boot ID to remain unchanged.")
			test.observeBoots(sbrparams.BPNNoReboot)

			By("Normally delete CR A. Require release of its restrictions.")
			test.releaseRequest(first)

			By("Record the current host boot ID. Create CR B with the same target name and a different UID.")
			second, secondDispatchedAt := test.request()
			Expect(second.GetUID()).NotTo(Equal(first.GetUID()))

			By("Require the common requested-remediation result for CR B against this new baseline.")
			secondBoot, secondRebootObservedAt := test.waitReboot(secondDispatchedAt)
			test.requireAgentRunning(secondRebootObservedAt)
			test.requireRequestedResult(second, secondDispatchedAt, secondRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = secondBoot
			test.checkFaultAfterReboot(storage)

			By("Retain CR B for NO_REBOOT. Require its resulting host boot ID to remain unchanged.")
			test.observeBoots(sbrparams.BPNNoReboot)

			By("Restore storage. Normally delete CR B and complete common cleanup.")
			test.restoreFault(storage)
			test.requireHealth(corev1.ConditionFalse)
			test.releaseRequest(second)
		})

		// TODO(RHWA-1839-17B): Repeat with more requested CRs during the same unresolved outage. This needs additional reboot cycles but no internal state assertions.
		It("RHWA-1839-17B A requested reboot remains permitted after an autonomous reboot — matrix row 17", reportxml.ID("RHWA-1839-17B"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17B")

			By("Complete autonomous preparation and confirm all observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.autonomousPreparation()

			By("Establish the first combined outage through Section 4.5. Observe the first changed host boot ID.")
			storage, api, from := test.combined()
			autonomousBoot, autonomousRebootObservedAt := test.waitReboot(from)
			test.nodes[0].boot = autonomousBoot
			test.checkFaultAfterReboot(storage, api)

			By("Keep both faults active for NO_REBOOT. Require no further observed reboot.")
			test.observeNoRebootWithoutRequests()

			By("Restore only API access. Keep storage unavailable and require True storage health when reporting resumes.")
			test.restoreFault(api)
			test.requireHealth(corev1.ConditionTrue, 0)
			test.requireAgentRunning(autonomousRebootObservedAt)

			By("Record the current host boot ID. Create a new valid target SBR CR.")
			request, requestDispatchedAt := test.request()

			By("Require its requested reboot and common terminal result despite the earlier autonomous attempt.")
			requestBoot, requestRebootObservedAt := test.waitReboot(requestDispatchedAt)
			test.requireAgentRunning(requestRebootObservedAt)
			test.requireRequestedResult(request, requestDispatchedAt, requestRebootObservedAt)
			test.requirePeerBoots()
			test.nodes[0].boot = requestBoot
			test.checkFaultAfterReboot(storage)

			By("Normally delete the CR with API access still available.")
			test.releaseRequest(request)

			By("Record the resulting host boot ID. Block target API access again while storage remains unavailable.")
			test.activate(api)

			By("Observe NO_REBOOT without another changed boot ID.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths. Complete common cleanup.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-17C): Repeat with several brief recoveries. This needs additional outage cycles and must still use bounded external observations.
		It("RHWA-1839-17C Brief recovery does not permit another autonomous reboot — matrix row 17", reportxml.ID("RHWA-1839-17C"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17C")

			By("Complete the baseline, autonomous preparation, and observer-readiness procedure.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.autonomousPreparation()

			By("Establish the first combined outage through Section 4.5. Observe a changed host boot ID.")
			storage, api, from := test.combined()
			test.nodes[0].boot, _ = test.waitReboot(from)
			test.checkFaultAfterReboot(storage, api)

			By("Keep combined loss active for NO_REBOOT after that boot. Require an unchanged new boot ID.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths. Start the brief recovery window at the first restoration command's dispatch time.")
			recoveryStart := time.Now()
			test.restoreFault(storage)
			test.restoreFault(api)

			By("Confirm restored API transport and False storage health within that 20s window.")
			confirmedFirst := test.healthyUntilDeadline(recoveryStart, recoveryStart.Add(sbrparams.BPNBriefRecovery))
			test.holdHealthyUntil(confirmedFirst, recoveryStart.Add(sbrparams.BPNBriefRecovery))

			By("Dispatch both fault commands at 20s. Confirm both faults before the window reaches 60s.")
			test.activate(storage)
			test.activate(api)
			if time.Since(recoveryStart) >= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: the possible healthy interval reached 60s")
			}

			By("Observe the same host boot ID for NO_REBOOT after confirmed combined loss.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths and complete common cleanup.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-17D): Repeat with the omitted five-minute default. This needs a longer run and is not covered by the configured one-minute result.
		It("RHWA-1839-17D Continuous recovery permits a later autonomous reboot — matrix row 17", reportxml.ID("RHWA-1839-17D"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17D")

			By("Complete the baseline and autonomous preparation. Confirm observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.autonomousPreparation()

			By("Establish the first combined outage through Section 4.5. Observe the first changed host boot ID.")
			storage, api, from := test.combined()
			test.nodes[0].boot, _ = test.waitReboot(from)
			test.checkFaultAfterReboot(storage, api)

			By("Keep the outage active for NO_REBOOT after that boot. Require an unchanged new boot ID.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths. Require False storage health and live API transport.")
			test.restoreFault(storage)
			test.restoreFault(api)
			test.requireHealth(corev1.ConditionFalse)

			By("Observe both healthy results continuously for 60s plus REARM_SETTLE, totaling 120s.")
			test.healthyContinuous(sbrparams.BPNRecoveryPeriod + sbrparams.BPNRearmSettle)

			By("Record the current host boot ID. Apply a new persistent combined outage.")
			test.activate(storage)
			test.finalBoots(0)
			test.activate(api)

			By("Require another changed host boot ID within REBOOT.")
			test.nodes[0].boot, _ = test.waitReboot(time.Now())
			test.checkFaultAfterReboot(storage, api)

			By("Keep that outage active for NO_REBOOT. Require no further observed reboot.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths and complete common cleanup.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
		})

		// TODO(RHWA-1839-17E): Interrupt recovery with a storage failure instead of API loss. This needs a bounded, reversible fault whose first failed probe occurs inside the shorter interval.
		It("RHWA-1839-17E An API interruption restarts the recovery interval — matrix row 17", reportxml.ID("RHWA-1839-17E"), FlakeAttempts(1), func() {
			test := newBPNFixture("RHWA-1839-17E")

			By("Complete autonomous preparation and confirm all observers are ready.")
			test.prepare()
			test.createPausedPolicy()
			test.requireComponentsReady()
			test.autonomousPreparation()

			By("Establish the first combined outage through Section 4.5. Observe the first changed host boot ID.")
			storage, api, from := test.combined()
			test.nodes[0].boot, _ = test.waitReboot(from)
			test.checkFaultAfterReboot(storage, api)

			By("Keep the outage active for NO_REBOOT. Require no further observed reboot.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths. Start the first recovery window at the first restoration command's dispatch time.")
			recoveryStart := time.Now()
			test.restoreFault(storage)
			test.restoreFault(api)

			By("Confirm False storage health and live API transport. Record when both results become available.")
			confirmedFirst := test.healthyUntilDeadline(recoveryStart, recoveryStart.Add(sbrparams.BPNRecoveryPart))
			test.holdHealthyUntil(confirmedFirst, recoveryStart.Add(sbrparams.BPNRecoveryPart))

			By("Dispatch the API fault at 40s. Keep confirmed API loss active for RECOVERY_BREAK=20s while storage remains available.")
			test.activate(api)
			if time.Since(recoveryStart) >= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: the first possible healthy interval reached 60s")
			}
			firstEnd := api.Evidence().DispatchedAt
			test.waitUntil(api.Evidence().ActiveAt.Add(sbrparams.BPNRecoveryBreak))

			By("Restore API access. Start the second recovery window at that command's dispatch time. Confirm both healthy results again.")
			secondStart := time.Now()
			test.restoreFault(api)
			confirmedSecond := test.healthyUntilDeadline(secondStart, secondStart.Add(sbrparams.BPNRecoveryPart))
			test.holdHealthyUntil(confirmedSecond, secondStart.Add(sbrparams.BPNRecoveryPart))

			By("Dispatch combined loss at the second window's 40s point. Require each possible healthy interval to remain below 60s.")
			secondEnd := time.Now()
			test.activate(storage)
			test.activate(api)
			if time.Since(secondStart) >= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: the second possible healthy interval reached 60s")
			}

			By("Require the confirmed healthy intervals to total more than 60s. Apply the bounds in Section 4.5.")
			confirmedTotal := firstEnd.Sub(confirmedFirst) + secondEnd.Sub(confirmedSecond)
			if confirmedTotal <= sbrparams.BPNRecoveryPeriod {
				Fail("INCONCLUSIVE: confirmed healthy time did not exceed 60s in total")
			}
			test.coverage(confirmedFirst, time.Now(), false)
			Expect(test.healthHistory(corev1.ConditionFalse, confirmedFirst, firstEnd, 0)).To(Succeed(), "First interval health")
			Expect(test.healthHistory(corev1.ConditionFalse, confirmedSecond, secondEnd, 0)).To(Succeed(), "Second interval")
			AddReportEntry("bpn-recovery-intervals", map[string]interface{}{
				"firstStart": recoveryStart, "firstConfirmed": confirmedFirst,
				"firstEnd": firstEnd, "secondStart": secondStart, "secondConfirmed": confirmedSecond,
				"secondEnd": secondEnd, "confirmedTotal": confirmedTotal.String(),
			})

			By("Observe the current host boot ID for NO_REBOOT without a further change.")
			test.observeNoRebootWithoutRequests()

			By("Restore both paths and complete common cleanup.")
			test.restoreFaults()
			test.requireHealth(corev1.ConditionFalse)
		})
	})
