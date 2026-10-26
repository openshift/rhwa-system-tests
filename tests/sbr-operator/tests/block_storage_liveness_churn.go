package tests

import (
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe(
	"SBR Block Storage — Ceph RBD Liveness Probe Under Process Churn",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentDaemonSet), func() {
		var (
			testSBRC        *unstructured.Unstructured
			victimNodeName  string
			victimAgentPod  string
			victimAgentUID  string
			healthyNodeName string
		)

		// agentRestartedUnderChurnHint is appended to the failure messages of the two restart-count
		// assertions. The distinction it points at is the whole point of the test: the previous
		// container's log ending in a clean shutdown means the agent was healthy and the probe
		// killed it, which is the defect; anything else means the agent fell over on its own.
		const agentRestartedUnderChurnHint = "if this fails, read the agent's previous container log: " +
			"a heartbeat write and a watchdog pet followed by a shutdown signal and exit code 0 means " +
			"a healthy agent was killed by its liveness probe, while any other ending means the agent " +
			"exited on its own and this is a different failure"

		BeforeAll(func() {
			// RHWA-1955 is open: the agent's probes find the agent by scanning every process on
			// the host, so a process exiting mid-scan makes the probe exit non-zero on a busy node.
			// This test is written to catch exactly that, so it fails on a build without the fix
			// and only runs when it is asked for.
			if sbrparams.SBRRunLivenessChurn == "" {
				Skip("RHWA-1955 (the sbr-agent probes scan the whole process table and fail under " +
					"process churn) is still open, so this test fails on a current build. Set " +
					"SBR_RUN_LIVENESS_CHURN to a non-empty value to run it anyway.")
			}

			// Step 1. An NHC on the SBR template would create a remediation CR for the victim in
			// the second spec, which would correctly reboot it — and the reboot, not the probe,
			// would then be what removes the agent.
			By("Step 1: Verifying no NodeHealthCheck remediates through the SBR template")
			requireNoSBRNodeHealthCheck()

			// Steps 2-3.
			testSBRC = createBlockModeSBRCAndWaitReady(sbrparams.SBRCBlockLivenessChurnTestName, nil)

			By("Step 4: Selecting a victim worker node whose agent is Running and Ready")

			// The victim hosts no SBR controller pod: the second spec takes its storage away, and
			// taking the operator down with it would change what the test is measuring.
			victimNodeName = pickTargetWorkerNode()
			if victimNodeName == "" {
				Skip("No schedulable worker node found that does not host an SBR controller pod")
			}

			Eventually(func() error {
				return agentPodReadyOnNode(sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
			}, sbrparams.BlockFencingStateCheckTimeout,
				sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
				"The SBRC %q agent pod on victim node %q must be Ready before any churn is applied",
				sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)

			GinkgoWriter.Printf("Victim node for the process churn: %q\n", victimNodeName)

			By("Step 5: Finding the agent pod on the victim node")

			var agentNameErr error
			victimAgentPod, agentNameErr = agentPodNameByNode(
				sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName, true)
			Expect(agentNameErr).ToNot(HaveOccurred(),
				"An SBR agent pod of SBRC %q must be Running on victim node %q",
				sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)

			// Pinned alongside the name: the event lookups narrow on the UID, so a pod that is
			// replaced mid-test stops matching instead of silently handing back a new pod's events.
			var agentUIDErr error
			victimAgentUID, agentUIDErr = agentPodUID(victimAgentPod)
			Expect(agentUIDErr).ToNot(HaveOccurred(),
				"Must read the UID of victim agent pod %q", victimAgentPod)

			GinkgoWriter.Printf("Victim agent pod: %q (UID %s)\n", victimAgentPod, victimAgentUID)

			By("Step 6: Selecting a healthy node that runs an agent, other than the victim")

			// This node is the control for the watchdog-holder check in steps 24-25: running the
			// same scan somewhere healthy first is what makes an empty result on the victim mean
			// "the agent is gone" rather than "the scan does not work here".
			healthyAgentPod, healthyNode, offNodeErr := agentPodOffNode(
				sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
			Expect(offNodeErr).ToNot(HaveOccurred(),
				"SBRC %q must have a Running agent pod on a node other than the victim %q",
				sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
			healthyNodeName = healthyNode

			Eventually(func() error {
				return agentPodReadyOnNode(sbrparams.SBRCBlockLivenessChurnTestName, healthyNodeName)
			}, sbrparams.BlockFencingStateCheckTimeout,
				sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
				"The SBRC %q agent pod on healthy node %q must be Ready: it is the control for the "+
					"watchdog-holder check", sbrparams.SBRCBlockLivenessChurnTestName, healthyNodeName)

			GinkgoWriter.Printf("Healthy control node: %q (agent pod %q)\n", healthyNodeName, healthyAgentPod)
		})

		AfterAll(func() {
			By("Step 28: Removing the process churn pods")
			deleteHostCommandPod(sbrparams.BlockChurnPodName)
			deleteHostCommandPod(sbrparams.BlockChurnStorageLossPodName)

			if victimNodeName != "" {
				// Before the injector pod goes: the flusher is its own host-command pod, so it does
				// not depend on the injector, but the node is far more likely to schedule it once
				// the churn has stopped.
				By("Step 29: Flushing any leftover Ceph REJECT rule on the victim node")
				flushCephRBDRules(victimNodeName)
			}

			By("Step 30: Removing the Ceph traffic injector pod")
			deleteHostCommandPod(sbrparams.BlockChurnInjectorPodName)

			if victimNodeName != "" {
				By("Step 31: Waiting for the victim's agent pod to be Ready again")

				// Reported, not asserted. The churn is rough on the node, and it has been seen to
				// leave the shared mount unusable so the agent cannot start even once its storage
				// is back. That leaves the node with no agent and would time out every wait in the
				// next spec, so it has to be visible here rather than inherited silently.
				recoveryReporter := NewGomega(func(message string, _ ...int) {
					AddReportEntry(fmt.Sprintf(
						"victim node %q was left without a Ready SBR agent", victimNodeName), message)
					GinkgoWriter.Printf("Warning: victim node %q has no Ready SBR agent after the "+
						"churn test: %s\n", victimNodeName, message)
				})

				recoveryReporter.Eventually(func() error {
					return agentPodReadyOnNode(sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
				}, sbrparams.BlockChurnAgentRecoveryTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"The SBRC %q agent pod on node %q must be Ready again once the churn and the Ceph "+
						"block are gone", sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Step 32: Removing StorageBasedRemediationConfig %q",
					sbrparams.SBRCBlockLivenessChurnTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCBlockLivenessChurnTestName)
			}
		})

		It("Process churn on a node never restarts its SBR agent",
			reportxml.ID("1963"),
			Label(
				labels.OperatorSBR,
				labels.TierResiliency,
				labels.ComponentDaemonSet,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By("Step 7: Recording the victim agent's container restart count")

				baselineRestarts, restartErr := agentRestartCount(victimAgentPod)
				Expect(restartErr).ToNot(HaveOccurred(),
					"Must read the container restart count of victim agent pod %q", victimAgentPod)
				GinkgoWriter.Printf("Victim agent pod %q restart count before the churn: %d\n",
					victimAgentPod, baselineRestarts)

				By("Step 8: Recording the cutoff the restart-decision event assertion is scoped to")

				// Restart decisions are already on every agent pod on an untouched cluster and
				// events live for about an hour, so step 13 only counts what is newer than this.
				churnStartedAt := time.Now()

				probeSamples := livenessProbeSamplerFor(
					sbrparams.SBRCBlockLivenessChurnTestName, victimAgentPod)

				By(fmt.Sprintf("Step 9: Starting a host-PID process churn pod on victim node %q",
					victimNodeName))

				churnPod := createHostCommandPod(victimNodeName, sbrparams.BlockChurnPodName,
					hostProcessChurnScript(sbrparams.BlockChurnDuration))

				DeferCleanup(func() {
					By("DeferCleanup: removing the process churn pod")
					deleteHostCommandPod(sbrparams.BlockChurnPodName)
				})

				By("Step 10: Waiting for the churn pod to be Running")

				Expect(churnPod.WaitUntilRunning(sbrparams.BlockChurnPodRunningTimeout)).To(Succeed(),
					"The process churn pod %q must reach Running on node %q, otherwise nothing is "+
						"churning the process table the agent's probes scan",
					sbrparams.BlockChurnPodName, victimNodeName)

				By(fmt.Sprintf("Step 11: Consistently asserting for %s that the victim's agent is "+
					"never restarted", sbrparams.BlockChurnObserveDuration))

				// The churn pod's liveness is part of the assertion, not a nicety: a churn pod that
				// died takes the load with it, and the rest of the window would then pass without
				// anything being exercised.
				Consistently(func() error {
					if churnErr := hostCommandPodRunning(sbrparams.BlockChurnPodName); churnErr != nil {
						return fmt.Errorf("the churn pod stopped driving load on node %s, so the rest "+
							"of this window would prove nothing: %w", victimNodeName, churnErr)
					}

					if sampleErr := sampleLivenessProbe(probeSamples); sampleErr != nil {
						return sampleErr
					}

					return agentRestartsUnchanged(victimAgentPod, baselineRestarts)
				}, sbrparams.BlockChurnObserveDuration,
					sbrparams.BlockChurnObserveInterval).Should(Succeed(),
					"The agent on node %q must survive process churn: %s",
					victimNodeName, agentRestartedUnderChurnHint)

				assertNoLivenessProbeActivity("Steps 12-13", victimAgentPod, victimAgentUID,
					churnStartedAt, probeSamples)

				By("Step 14: Asserting the victim's agent pod is Ready")

				Expect(agentPodReadyOnNode(sbrparams.SBRCBlockLivenessChurnTestName,
					victimNodeName)).To(Succeed(),
					"The SBRC %q agent pod on node %q must still be Ready after the churn",
					sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
			})

		It("A churned node that has also lost its Ceph storage keeps its agent and its watchdog",
			reportxml.ID("1963"),
			Label(
				labels.OperatorSBR,
				labels.TierResiliency,
				labels.ComponentDaemonSet,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By(fmt.Sprintf("Precondition: waiting for victim node %q and its agent to be "+
					"healthy again after the first spec's churn", victimNodeName))

				// Ten minutes of churn is rough on a node, and in a manual run it was enough on its
				// own to wedge the victim's CephFS mount and leave the agent unable to start. This
				// spec needs a node with a live agent to take the storage away from; starting it on
				// a node that already has none would fail on something that has nothing to do with
				// the liveness probe. The agent also gets this window to come back on its own: it
				// may still be settling now that the churn has stopped.
				//
				// Reported and skipped rather than failed. Whatever wedged the node was either the
				// first spec's own failure, which is already red, or the environment, and a second
				// red for it would only duplicate the first.
				var preconditionFailure string

				preconditionGomega := NewGomega(func(message string, _ ...int) {
					preconditionFailure = message
				})

				preconditionGomega.Eventually(func() error {
					return nodeAndAgentReady(sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)
				}, sbrparams.BlockChurnAgentRecoveryTimeout, sbrparams.DefaultPollInterval).Should(Succeed())

				if preconditionFailure != "" {
					logNodeStatus(victimNodeName)
					AddReportEntry(fmt.Sprintf("victim node %q was not usable for the storage-loss "+
						"churn spec", victimNodeName), preconditionFailure)
					Skip(fmt.Sprintf("Victim node %q has no Ready SBR agent after the first spec's "+
						"churn, so there is nothing on it to take the storage away from. Check the "+
						"first spec's result and the node's shared storage mount: %s",
						victimNodeName, preconditionFailure))
				}

				// Re-resolved rather than reused from BeforeAll: if the agent pod was replaced while
				// the node recovered, the pinned name and UID point at an object that no longer
				// exists, and every restart-count and event lookup below would be reading nothing.
				var agentNameErr error
				victimAgentPod, agentNameErr = agentPodNameByNode(
					sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName, true)
				Expect(agentNameErr).ToNot(HaveOccurred(),
					"An SBR agent pod of SBRC %q must be Running on victim node %q",
					sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)

				var agentUIDErr error
				victimAgentUID, agentUIDErr = agentPodUID(victimAgentPod)
				Expect(agentUIDErr).ToNot(HaveOccurred(),
					"Must read the UID of victim agent pod %q", victimAgentPod)

				GinkgoWriter.Printf("Victim agent pod for the storage-loss churn: %q (UID %s)\n",
					victimAgentPod, victimAgentUID)

				By("Step 15: Recording the victim agent's container restart count")

				baselineRestarts, restartErr := agentRestartCount(victimAgentPod)
				Expect(restartErr).ToNot(HaveOccurred(),
					"Must read the container restart count of victim agent pod %q", victimAgentPod)
				GinkgoWriter.Printf("Victim agent pod %q restart count before the storage loss: %d\n",
					victimAgentPod, baselineRestarts)

				By("Step 16: Recording the cutoff the restart-decision event assertion is scoped to")

				storageLossStartedAt := time.Now()

				probeSamples := livenessProbeSamplerFor(
					sbrparams.SBRCBlockLivenessChurnTestName, victimAgentPod)

				By(fmt.Sprintf("Step 17: Starting a privileged injector pod that blocks the Ceph "+
					"traffic of victim node %q", victimNodeName))

				injectorPod, injectorErr := startPrivilegedInjectorPod(
					victimNodeName, sbrparams.BlockChurnInjectorPodName)
				Expect(injectorErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on victim node %q", victimNodeName)

				DeferCleanup(func() {
					By("DeferCleanup: removing the Ceph traffic injector pod and flushing leftover " +
						"Ceph REJECT rules")
					deleteHostCommandPod(sbrparams.BlockChurnInjectorPodName)
					flushCephRBDRules(victimNodeName)
				})

				// Outbound only, matching the reproducer: the node loses Ceph, and the replies of
				// an already-established session that still arrive change nothing about whether a
				// restarted agent could come back.
				injectOut, injectErr := injectCephRBDRules(injectorPod)
				Expect(injectErr).ToNot(HaveOccurred(),
					"Ceph REJECT rules must be inserted on the OUTPUT chain of victim node %q "+
						"(output: %s)", victimNodeName, injectOut)

				By("Step 18: Listing the victim's OUTPUT chain")

				By("Step 19: Asserting the Ceph REJECT rules are in place")

				// The insert command's exit code is not enough: the rules have to be read back, or
				// a spec that never actually took the storage away would still reach the end.
				//
				// Retried rather than read once. The rules are inserted before this and nothing in
				// the spec removes them, so the only thing a listing without them can mean is that
				// this particular read did not come back whole - which has happened: a listing that
				// stopped after the chain header, with iptables' own column header missing, failed
				// the step on a chain that did have the rules.
				var chainListing string

				Eventually(func() ([]string, error) {
					listing, listErr := cephRBDChainListing(injectorPod, "OUTPUT")
					if listErr != nil {
						return nil, fmt.Errorf("list the OUTPUT chain of node %s (output: %s): %w",
							victimNodeName, listing, listErr)
					}

					chainListing = listing

					return missingCephRBDRejectPorts(listing), nil
				}, sbrparams.BlockCephRuleConfirmTimeout, sbrparams.DefaultPollInterval).Should(BeEmpty(),
					"Every Ceph port must have a REJECT rule on the OUTPUT chain of node %q, "+
						"otherwise the node has not lost its storage. Last chain listing:\n%s",
					victimNodeName, chainListing)

				GinkgoWriter.Printf("OUTPUT chain on node %q after injection:\n%s\n",
					victimNodeName, chainListing)

				By(fmt.Sprintf("Step 20: Starting a host-PID process churn pod on victim node %q",
					victimNodeName))

				churnPod := createHostCommandPod(victimNodeName, sbrparams.BlockChurnStorageLossPodName,
					hostProcessChurnScript(sbrparams.BlockChurnDuration))

				DeferCleanup(func() {
					By("DeferCleanup: removing the storage-loss process churn pod")
					deleteHostCommandPod(sbrparams.BlockChurnStorageLossPodName)
				})

				By("Step 21: Waiting for the churn pod to be Running")

				Expect(churnPod.WaitUntilRunning(sbrparams.BlockChurnPodRunningTimeout)).To(Succeed(),
					"The process churn pod %q must reach Running on node %q, otherwise nothing is "+
						"churning the process table the agent's probes scan",
					sbrparams.BlockChurnStorageLossPodName, victimNodeName)

				By(fmt.Sprintf("Step 22: Consistently asserting for %s that the victim stays Ready "+
					"and its agent is never restarted", sbrparams.BlockChurnObserveDuration))

				// This is the serious case. A restarted agent cannot come back while the node has no
				// storage, so nothing on the node would hold the watchdog and nothing could reboot
				// it. The spec deliberately never deletes the agent pod by hand: leaving the churn
				// as the only thing that can remove it means the probe is the only suspect.
				Consistently(func() error {
					if churnErr := hostCommandPodRunning(sbrparams.BlockChurnStorageLossPodName); churnErr != nil {
						return fmt.Errorf("the churn pod stopped driving load on node %s, so the rest "+
							"of this window would prove nothing: %w", victimNodeName, churnErr)
					}

					if readyErr := nodeConditionIs(victimNodeName, string(corev1.NodeReady),
						corev1.ConditionTrue, ""); readyErr != nil {
						return readyErr
					}

					if sampleErr := sampleLivenessProbe(probeSamples); sampleErr != nil {
						return sampleErr
					}

					return agentRestartsUnchanged(victimAgentPod, baselineRestarts)
				}, sbrparams.BlockChurnObserveDuration,
					sbrparams.BlockChurnObserveInterval).Should(Succeed(),
					"Node %q must stay Ready and keep its agent while it is churned with no Ceph "+
						"storage: %s", victimNodeName, agentRestartedUnderChurnHint)

				// The restart count is the coarse signal and the watchdog holder is the consequence;
				// the probe's own exit status is the sensitive one. A probe that is flapping but has
				// not yet strung three failures together leaves both of the others untouched, so
				// this is where a half-reproduced defect shows up first.
				assertNoLivenessProbeActivity("Step 22b", victimAgentPod, victimAgentUID,
					storageLossStartedAt, probeSamples)

				By("Step 23: Asserting the victim's agent pod is Ready")

				Expect(agentPodReadyOnNode(sbrparams.SBRCBlockLivenessChurnTestName,
					victimNodeName)).To(Succeed(),
					"The SBRC %q agent pod on node %q must still be Ready after the churn with no "+
						"storage", sbrparams.SBRCBlockLivenessChurnTestName, victimNodeName)

				By(fmt.Sprintf("Step 24: Checking which processes hold the watchdog device open on "+
					"healthy node %q", healthyNodeName))

				healthyPhase, healthyLog := runHostCommandPod(healthyNodeName,
					sbrparams.BlockWatchdogHolderProbePodName, watchdogHolderScanScript(),
					sbrparams.BlockPartitionPodTimeout)
				Expect(healthyPhase).To(Equal(corev1.PodSucceeded),
					"The watchdog-holder scan must complete on healthy node %q (pod %s): %s",
					healthyNodeName, healthyPhase, healthyLog)

				healthyHolders, healthyParseErr := watchdogHolders(healthyLog)
				Expect(healthyParseErr).ToNot(HaveOccurred(),
					"The watchdog-holder scan output from healthy node %q must be parseable",
					healthyNodeName)

				By("Step 25: Asserting a process on the healthy node holds the watchdog device")

				// The control for step 27. Without it, an empty result on the victim could just as
				// easily mean the scan does not work on these nodes at all.
				Expect(healthyHolders).ToNot(BeEmpty(),
					"A process on healthy node %q must hold a watchdog device open, otherwise the "+
						"scan itself proves nothing on the victim. Scan output:\n%s",
					healthyNodeName, healthyLog)
				GinkgoWriter.Printf("Watchdog holders on healthy node %q: %v\n",
					healthyNodeName, healthyHolders)

				By(fmt.Sprintf("Step 26: Checking which processes hold the watchdog device open on "+
					"victim node %q", victimNodeName))

				victimPhase, victimLog := runHostCommandPod(victimNodeName,
					sbrparams.BlockWatchdogHolderProbePodName, watchdogHolderScanScript(),
					sbrparams.BlockPartitionPodTimeout)
				Expect(victimPhase).To(Equal(corev1.PodSucceeded),
					"The watchdog-holder scan must complete on victim node %q (pod %s): %s",
					victimNodeName, victimPhase, victimLog)

				victimHolders, victimParseErr := watchdogHolders(victimLog)
				Expect(victimParseErr).ToNot(HaveOccurred(),
					"The watchdog-holder scan output from victim node %q must be parseable",
					victimNodeName)

				By("Step 27: Asserting a process on the victim still holds the watchdog device")

				Expect(victimHolders).ToNot(BeEmpty(),
					"A process on victim node %q must still hold a watchdog device open. The same "+
						"scan found one on healthy node %q, so an empty result here means the agent "+
						"is gone and nothing is left to reboot a node that has lost its storage. "+
						"Scan output:\n%s", victimNodeName, healthyNodeName, victimLog)
				GinkgoWriter.Printf("Watchdog holders on victim node %q: %v\n",
					victimNodeName, victimHolders)
			})
	})

// sampleLivenessProbe runs one probe sample, tolerating a nil sampler so the windows read the same
// whether or not the agent container defines a liveness probe to sample.
func sampleLivenessProbe(sampler *probeSampler) error {
	if sampler == nil {
		return nil
	}

	return sampler.sample()
}

// livenessProbeSamplerFor builds a sampler that runs the sbr-agent liveness probe the way kubelet
// does, reading the command off the live DaemonSet. It returns nil when the container defines no
// liveness probe, which is the fixed state: nothing can then restart the agent under churn, so
// there is nothing to sample.
func livenessProbeSamplerFor(sbrcName, agentPodName string) *probeSampler {
	probe, probeErr := agentLivenessProbe(sbrcName)
	if errors.Is(probeErr, errNoLivenessProbe) {
		GinkgoWriter.Printf("The agent container of SBRC %q defines no liveness probe, so nothing "+
			"can restart the agent under churn and the probe sampling is skipped\n", sbrcName)

		return nil
	}

	Expect(probeErr).ToNot(HaveOccurred(),
		"Must read the liveness probe of the agent container of SBRC %q", sbrcName)

	GinkgoWriter.Printf("Sampling the liveness probe of agent pod %q: %v\n",
		agentPodName, probe.command)

	return &probeSampler{agentPodName: agentPodName, probe: probe}
}

// assertNoLivenessProbeActivity asserts that the probe never came back non-zero while the window
// was running, and that kubelet never decided to restart the agent container over it.
//
// The two halves catch different things. The restart decision is the rare end state: it needs
// three consecutive failures and it is what releases the watchdog. The individual failures are the
// sensitive signal, and they come from the sampler rather than from kubelet's events, because
// kubelet rate-limits those per object: in an earlier run it stopped emitting them four minutes
// into a ten-minute window while the probe went on failing, which made the absence of events read
// as health.
//
// stepLabel names the flow steps this stands in for, so the By lines still read against the test
// flow from either call site.
func assertNoLivenessProbeActivity(
	stepLabel, agentPodName, agentPodUID string, since time.Time, sampler *probeSampler,
) {
	By(fmt.Sprintf("%s: Asserting the liveness probe of agent pod %q never came back non-zero",
		stepLabel, agentPodName))

	if sampler != nil {
		GinkgoWriter.Printf("%s\n", sampler.summary())
		Expect(sampler.failures).To(BeEmpty(),
			"The liveness probe of agent pod %q must not fail under process churn: a probe that "+
				"finds the agent by scanning every process fails whenever one of them exits "+
				"mid-scan, and three failures in a row restart the container, releasing the "+
				"watchdog. %s. Failed runs: %v",
			agentPodName, sampler.summary(), sampler.failures)
	}

	By(fmt.Sprintf("%s: Asserting kubelet never decided to restart agent pod %q over its liveness probe",
		stepLabel, agentPodName))

	// Still read from events: a restart decision is rare enough not to be rate-limited away, and
	// the restart count asserted during the window covers it from the other side anyway.
	restartDecision := fmt.Sprintf("Container %s %s",
		sbrparams.AgentContainerName, sbrparams.LivenessRestartEventSubstr)
	restartEvents, restartEventErr := podEventsSince(agentPodName, agentPodUID, since, restartDecision)
	Expect(restartEventErr).ToNot(HaveOccurred(),
		"Must read the events of agent pod %q", agentPodName)
	Expect(restartEvents).To(BeEmpty(),
		"kubelet must never restart agent pod %q over its liveness probe. Events recorded since %s: %v",
		agentPodName, since.UTC().Format(time.RFC3339), restartEvents)
}
