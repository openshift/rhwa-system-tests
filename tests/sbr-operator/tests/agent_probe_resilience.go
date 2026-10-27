package tests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// rhwa1955GateEnvVar gates this spec while RHWA-1955 is open. The agent DaemonSet's liveness,
// readiness and startup probes decide the agent is alive by grepping every process on the host
// (the pod runs with hostPID), so a process that exits mid-scan makes grep exit 2 and the probe
// fail on a healthy agent. Three such checks in a row and kubelet restarts it.
const rhwa1955GateEnvVar = "SBR_TEST_RHWA1955"

// agentProbeBaseline is one SBR agent's identity, container restart count and probe complaints
// before the churn starts. The pod name is recorded alongside the count because a replaced pod
// resets the counter, so an unchanged count on a different pod would read as "never restarted".
// The probe events are recorded rather than assumed absent: a pod can legitimately collect a
// readiness complaint while it is still starting, and the test is about what the churn adds.
type agentProbeBaseline struct {
	nodeName     string
	agentPodName string
	agentPodUID  string
	restarts     int32
	probeEvents  []string
}

// processChurnScript spawns short-lived processes in bursts for the given duration and then exits.
// The pod runs with hostPID, so these are host processes: they appear in and disappear from the
// host's /proc while an agent probe is scanning it, which is the race RHWA-1955 is about. Each
// burst is reaped with wait before the next one starts, so the process list churns continuously
// without the unbounded growth a plain fork loop would cause on the node under test.
func processChurnScript(duration time.Duration, burst int) string {
	return fmt.Sprintf(`end=$(( $(date +%%s) + %d ))
spawned=0
while [ "$(date +%%s)" -lt "$end" ]; do
  started=0
  while [ "$started" -lt %d ]; do
    /bin/true &
    started=$((started + 1))
  done
  wait
  spawned=$((spawned + %d))
done
echo "spawned $spawned short-lived processes"
`, int(duration.Seconds()), burst, burst)
}

// watchdogHolderScript reports every host process holding a watchdog device open. The pod runs
// with hostPID, so /proc is the host's. It always ends with "checked" so a node where nothing holds
// the device is distinguishable from a script that never ran.
func watchdogHolderScript() string {
	return `for proc in /proc/[0-9]*; do
  ls -l "$proc/fd" 2>/dev/null | grep -q watchdog && echo "held by $proc"
done
echo checked
`
}

var _ = Describe(
	"SBR Agent Probe Resilience — a healthy agent must survive host process churn",
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorSBR, labels.ComponentDaemonSet), func() {
		var (
			testSBRC       *unstructured.Unstructured
			targetNodeName string
		)

		BeforeAll(func() {
			// Checked before anything is created: provisioning a Ceph RBD SBRC and its device-init
			// Job is expensive, and a gated-off run must not pay for it.
			if os.Getenv(rhwa1955GateEnvVar) != "true" {
				Skip(fmt.Sprintf("RHWA-1955 (sbr-agent liveness probe restarts a healthy agent) is not "+
					"fixed in every build; set %s=true to enable this check", rhwa1955GateEnvVar))
			}

			By("Checking the NodeHealthCheck CRD is installed")

			if !isNHCCRDInstalled() {
				Skip("NodeHealthCheck CRD not found; NHC operator not installed — skipping agent probe test")
			}

			// Removed before requireNoSBRNodeHealthCheck, which would otherwise see this test's own
			// NodeHealthCheck from a crashed prior run and skip the test.
			By("Removing any NodeHealthCheck left by a previous run of this test")
			deleteNHCAndWait(sbrparams.NHCAgentProbeTestName)

			By("Verifying no other NodeHealthCheck already remediates through the SBR template")
			requireNoSBRNodeHealthCheck()

			// The minimum sbrTimeoutSeconds (10) halves to a 5s heartbeat interval, so a peer reaches
			// a storage-loss verdict within the injection wait rather than at the edge of it. The
			// SBRStorageUnhealthy condition cycles True -> Unknown/GivingAgentChance roughly every
			// 30s at this setting, against NHC's 30s unhealthy duration, but fencing is proven well
			// inside the first True window: measured on a 6-node cluster, detection took 41s, NHC
			// created the CR 31s later, and FencingSucceeded/Succeeded followed within 6s.
			testSBRC = setupBlockModeSBRCWithSpec(sbrparams.SBRCAgentProbeTestName,
				map[string]interface{}{"sbrTimeoutSeconds": int64(sbrparams.SBRCTimeoutSecondsMin)})

			By("Asserting the SBRC reports concurrent-writeable shared storage")

			Eventually(func() (bool, error) {
				return sbrcConcurrentWriteable(sbrparams.SBRCAgentProbeTestName)
			}, sbrparams.StorageInjectionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
				"SBRC %q must report status.storageValidation.concurrentWriteable=true before fencing "+
					"can proceed", sbrparams.SBRCAgentProbeTestName)

			By("Selecting a target worker node that hosts no SBR controller pod")

			targetNodeName = pickTargetWorkerNode()
			if targetNodeName == "" {
				Skip("No schedulable worker node found that does not host an SBR controller pod")
			}

			GinkgoWriter.Printf("Target node for the agent probe test: %q\n", targetNodeName)
		})

		AfterAll(func() {
			// Everything here is guarded on BeforeAll having run to completion, which is what setting
			// targetNodeName means. A run that skipped on the env gate or a missing CRD created
			// nothing, and cleanupNHCCR polls its full timeout against a cluster with no NHC CRD.
			if targetNodeName != "" {
				By("Removing the NodeHealthCheck CR")
				cleanupNHCCR(context.Background(), sbrparams.NHCAgentProbeTestName)

				By("Removing the churn, watchdog and injector pods if still present")
				deleteHostCommandPod(sbrparams.AgentProbeChurnPodName)
				deleteHostCommandPod(sbrparams.AgentProbeWatchdogPodName)
				deleteHostCommandPod(sbrparams.AgentProbeInjectorPodName)

				By("Flushing any Ceph REJECT rule left on the target node")
				flushCephRBDRules(targetNodeName)

				By("Removing any StorageBasedRemediation CR left for the target node")
				cleanupSBRCR(context.Background(), targetNodeName)

				By("Uncordoning the target node if it is still cordoned")
				uncordonNodeIfNeeded(targetNodeName)
			}

			if testSBRC != nil {
				By(fmt.Sprintf("Removing StorageBasedRemediationConfig %q", sbrparams.SBRCAgentProbeTestName))
				deleteSBRCAndWait(testSBRC, sbrparams.SBRCAgentProbeTestName)
			}
		})

		It("Agent probe: host process churn must not restart a healthy agent, and a node reported "+
			"fenced must have rebooted",
			reportxml.ID("1965"),
			Label(
				labels.OperatorSBR,
				labels.TierResiliency,
				labels.ComponentDaemonSet,
				labels.ComponentRemediation,
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.FrequencyNightly,
			), func() {
				By("Asserting the agent container defines no liveness or startup probe")
				assertAgentProbesCannotMisfire(sbrparams.SBRCAgentProbeTestName)

				By("Recording every agent pod's container restart count before the churn")

				baselines := captureAgentProbeBaselines()
				targetBaseline, found := baselineForNode(baselines, targetNodeName)
				Expect(found).To(BeTrue(),
					"SBRC %q must have a Running agent pod on target node %q",
					sbrparams.SBRCAgentProbeTestName, targetNodeName)

				By(fmt.Sprintf("Starting a host-PID pod that spawns short-lived processes on node %q",
					targetNodeName))

				churnPod := createHostCommandPod(targetNodeName, sbrparams.AgentProbeChurnPodName,
					processChurnScript(sbrparams.AgentProbeChurnDuration, sbrparams.AgentProbeChurnBurst))

				DeferCleanup(func() {
					By("DeferCleanup: removing the process churn pod")
					deleteHostCommandPod(sbrparams.AgentProbeChurnPodName)
				})

				Expect(churnPod.WaitUntilRunning(medik8sparams.DefaultTimeout)).To(Succeed(),
					"Process churn pod %q must start on node %q",
					sbrparams.AgentProbeChurnPodName, targetNodeName)

				By(fmt.Sprintf("Consistently asserting for %s that no agent pod is restarted while the "+
					"target node's process list churns", sbrparams.AgentProbeNoRestartDuration))

				Consistently(func() error {
					return agentsNotRestarted(baselines)
				}, sbrparams.AgentProbeNoRestartDuration,
					sbrparams.AgentProbeNoRestartInterval).Should(Succeed(),
					"Host process churn must not restart any SBR agent: the probe scans every process "+
						"on the host and must not read a process exiting mid-scan as a dead agent (RHWA-1955)")

				By("Asserting no agent pod recorded a failed-probe event during the churn")
				assertNoAgentProbeEvents(baselines)

				By(fmt.Sprintf("Asserting a process on node %q still holds the watchdog device open",
					targetNodeName))

				// The control for the reading taken once the node is cut off from Ceph: an agent that
				// survived the churn is still the process arming the watchdog, so a later "nothing holds
				// it" is a real release rather than a check that reports nothing everywhere.
				assertWatchdogHeldOnNode(targetNodeName)

				By("Creating the NodeHealthCheck CR that remediates SBRStorageUnhealthy nodes through SBR")

				// Created here rather than in BeforeAll so the no-restart window above runs with nothing
				// that could fence the churned node, and maxUnhealthy: 1 rather than the shared builder's
				// minHealthy: "51%", which can block remediation on a small cluster.
				ensureSBRTemplate()
				nhc := buildNHCWithMaxUnhealthy(sbrparams.NHCAgentProbeTestName, 1)
				Expect(APIClient.Create(context.TODO(), nhc)).To(Succeed(),
					"NodeHealthCheck CR %q must be created", sbrparams.NHCAgentProbeTestName)

				DeferCleanup(func() {
					By("DeferCleanup: removing the NodeHealthCheck CR")
					cleanupNHCCR(context.Background(), sbrparams.NHCAgentProbeTestName)
				})

				By("Recording the target node's boot-id before its Ceph traffic is blocked")

				preFenceBootID, bootErr := getNodeBootID(targetNodeName)
				Expect(bootErr).ToNot(HaveOccurred(),
					"Must read the boot-id of node %q before its Ceph traffic is blocked", targetNodeName)
				GinkgoWriter.Printf("Pre-fencing boot-id of node %q: %s\n", targetNodeName, preFenceBootID)

				By(fmt.Sprintf("Blocking Ceph ports on node %q while the process churn continues",
					targetNodeName))

				injectorPod, injectorErr := startPrivilegedInjectorPod(
					targetNodeName, sbrparams.AgentProbeInjectorPodName)
				Expect(injectorErr).ToNot(HaveOccurred(),
					"Privileged injector pod must start on node %q", targetNodeName)

				DeferCleanup(func() {
					By("DeferCleanup: removing the injector pod and flushing leftover Ceph REJECT rules")
					deleteHostCommandPod(sbrparams.AgentProbeInjectorPodName)
					flushCephRBDRules(targetNodeName)
				})

				injectOut, injectErr := injectCephRBDRules(injectorPod)
				Expect(injectErr).ToNot(HaveOccurred(),
					"Ceph REJECT rules must be inserted on node %q (output: %s)", targetNodeName, injectOut)

				// From here the target cannot reach the shared device, so its own agent is the only thing
				// that can reboot it: it holds the watchdog, sees the remediation for its node and stops
				// petting. A kubelet restart in this window releases the watchdog and the replacement
				// cannot complete preflight without the storage, which is how RHWA-1955 turns a probe
				// misread into a node the cluster calls fenced while it is still running. Every wait
				// below therefore fails fast if the target's agent is restarted rather than waiting out
				// its own timeout on a scenario that can no longer end correctly.
				probeGuard := func() error {
					restartErr := targetAgentRestartedWhileUp(targetBaseline, preFenceBootID)
					if restartErr != nil {
						return StopTrying(fmt.Sprintf("RHWA-1955: kubelet restarted the SBR agent on "+
							"node %q while it was cut off from Ceph, so nothing is left to pet or release "+
							"the watchdog and the node can no longer fence itself", targetNodeName)).
							Wrap(restartErr)
					}

					return nil
				}

				By(fmt.Sprintf("Waiting for SBRStorageUnhealthy=True (reason %s) on the target node",
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout))

				Eventually(func() error {
					if guardErr := probeGuard(); guardErr != nil {
						return guardErr
					}

					return nodeConditionIs(targetNodeName, sbrparams.SBRStorageUnhealthyCondition,
						corev1.ConditionTrue, sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)
				}, sbrparams.StorageInjectionTimeout, sbrparams.StorageInjectionPollInterval).Should(Succeed(),
					"A peer must report node %q %s=True (reason %s) once its Ceph traffic is blocked",
					targetNodeName, sbrparams.SBRStorageUnhealthyCondition,
					sbrparams.SBRStorageUnhealthyReasonHeartbeatTimeout)

				By("Waiting for NHC to create a StorageBasedRemediation CR for the target node")

				Eventually(func() error {
					if guardErr := probeGuard(); guardErr != nil {
						return guardErr
					}

					_, pullErr := pullSBRCR(targetNodeName)
					if k8serrors.IsNotFound(pullErr) {
						return fmt.Errorf("StorageBasedRemediation/%s not yet created by NHC", targetNodeName)
					}

					return pullErr
				}, sbrparams.NHCSBRCRCreationTimeout, sbrparams.NHCSBRCRCreationPollInterval).Should(Succeed(),
					"NHC must create a StorageBasedRemediation CR for node %q", targetNodeName)

				By("Waiting for FencingSucceeded=True and Succeeded=True on the target's " +
					"StorageBasedRemediation CR")

				Eventually(func() error {
					if guardErr := probeGuard(); guardErr != nil {
						return guardErr
					}

					_, provenErr := fencingProven(targetNodeName)

					return provenErr
				}, sbrparams.FencingProvenTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"StorageBasedRemediation/%s must reach %s=True and %s=True",
					targetNodeName, sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition)

				By("Asserting the node reported fenced actually rebooted (its boot-id changed)")

				// The assertion the bug report's end state fails: FencingSucceeded=True, Succeeded=True
				// and the out-of-service taint applied, while the node kept its boot-id and stayed up.
				// A node the cluster has declared dead and moved work off must really be gone.
				Eventually(func() error {
					bootID, currentBootErr := getNodeBootID(targetNodeName)
					if currentBootErr != nil {
						// The node is mid-reboot and its kubelet has stopped reporting: keep waiting.
						return currentBootErr
					}

					if bootID == preFenceBootID {
						return fmt.Errorf("node %s still runs boot-id %s", targetNodeName, preFenceBootID)
					}

					GinkgoWriter.Printf("Boot-id of node %q changed: %q -> %q (it rebooted)\n",
						targetNodeName, preFenceBootID, bootID)

					return nil
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q is reported fenced (%s=True, %s=True) and must therefore have rebooted, but "+
						"it kept boot-id %q: the cluster has declared a running node dead (RHWA-1955)",
					targetNodeName, sbrparams.FencingSucceededCondition, sbrparams.SucceededCondition,
					preFenceBootID)

				By(fmt.Sprintf("Waiting for node %q to come back Ready with a Ready agent pod",
					targetNodeName))

				Eventually(func() error {
					return nodeConditionIs(targetNodeName, string(corev1.NodeReady), corev1.ConditionTrue, "")
				}, sbrparams.NodeRebootTimeout, sbrparams.NodeRebootPollInterval).Should(Succeed(),
					"Node %q must return Ready after the fencing reboot", targetNodeName)

				// The reboot clears the host's iptables rules, so the agent can reach Ceph again and the
				// SBRC's own agent pod on the node reaches Ready, which is gated on its preflight.
				Eventually(func() error {
					return agentPodReadyOnNode(sbrparams.SBRCAgentProbeTestName, targetNodeName)
				}, sbrparams.BlockFencingStateCheckTimeout,
					sbrparams.BlockFencingStateCheckInterval).Should(Succeed(),
					"The SBRC %q agent pod on node %q must become Ready after it rebooted",
					sbrparams.SBRCAgentProbeTestName, targetNodeName)

				By("Waiting for NHC to delete the StorageBasedRemediation CR and the out-of-service " +
					"taint to be removed")

				Eventually(func() error {
					_, pullErr := pullSBRCR(targetNodeName)
					if pullErr == nil {
						return fmt.Errorf("StorageBasedRemediation/%s still exists; waiting for NHC cleanup",
							targetNodeName)
					}

					if !k8serrors.IsNotFound(pullErr) {
						return pullErr
					}

					node, getErr := APIClient.CoreV1Interface.Nodes().Get(
						context.TODO(), targetNodeName, metav1.GetOptions{})
					if getErr != nil {
						return fmt.Errorf("get node %s: %w", targetNodeName, getErr)
					}

					if hasOutOfServiceTaint(node) {
						return fmt.Errorf("node %s still carries the %s taint",
							targetNodeName, sbrparams.OutOfServiceTaintKey)
					}

					if !isNodeSchedulable(node) {
						return fmt.Errorf("node %s is not schedulable yet (cordoned or tainted)", targetNodeName)
					}

					return nil
				}, sbrparams.SBRCRCleanupTimeout, sbrparams.SBRCRCleanupPollInterval).Should(Succeed(),
					"NHC must delete the StorageBasedRemediation CR for node %q and the %s taint must be "+
						"removed", targetNodeName, sbrparams.OutOfServiceTaintKey)

				GinkgoWriter.Printf("Agent probe resilience complete for node %q\n", targetNodeName)
			})
	})

// assertAgentProbesCannotMisfire reads the SBRC's agent DaemonSet and asserts its probes cannot
// report a healthy agent as dead. RHWA-1955 is fixed by dropping the liveness and startup probes
// altogether — the agent is the container's entrypoint, so kubelet restarts it when it exits, and
// the watchdog is the real liveness supervisor for a fencing agent. Any probe that is left must not
// decide the question by scanning /proc, which is the scan that misreads a process exiting mid-scan
// as a missing agent.
func assertAgentProbesCannotMisfire(sbrcName string) {
	dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrcName
	agentDS, dsErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
		context.TODO(), dsName, metav1.GetOptions{})
	Expect(dsErr).ToNot(HaveOccurred(), "Must read the agent DaemonSet %q", dsName)
	container := agentContainerOf(agentDS)
	Expect(container).ToNot(BeNil(),
		"Agent DaemonSet %q must define a %q container", dsName, sbrparams.AgentContainerName)
	GinkgoWriter.Printf("Agent container probes: liveness=%s startup=%s readiness=%s\n",
		probeSummary(container.LivenessProbe), probeSummary(container.StartupProbe),
		probeSummary(container.ReadinessProbe))
	Expect(container.LivenessProbe).To(BeNil(),
		"Agent container %q in DaemonSet %q must define no liveness probe: the agent is the "+
			"container entrypoint, so kubelet restarts it when it exits, and a probe that cannot "+
			"detect a dead agent can only produce false positives (RHWA-1955)",
		sbrparams.AgentContainerName, dsName)
	Expect(container.StartupProbe).To(BeNil(),
		"Agent container %q in DaemonSet %q must define no startup probe (RHWA-1955)",
		sbrparams.AgentContainerName, dsName)
	Expect(probeScansProc(container.ReadinessProbe)).To(BeFalse(),
		"The readiness probe of agent container %q in DaemonSet %q must not decide the agent is "+
			"alive by scanning /proc: the pod runs with hostPID, so a process exiting mid-scan makes "+
			"the scan fail on a healthy agent (RHWA-1955). Probe: %s",
		sbrparams.AgentContainerName, dsName, probeSummary(container.ReadinessProbe))
}

// agentContainerOf returns the SBR agent container of an agent DaemonSet, or nil when the
// DaemonSet does not define one.
func agentContainerOf(agentDS *appsv1.DaemonSet) *corev1.Container {
	for idx := range agentDS.Spec.Template.Spec.Containers {
		container := &agentDS.Spec.Template.Spec.Containers[idx]
		if container.Name == sbrparams.AgentContainerName {
			return container
		}
	}

	return nil
}

// probeSummary renders a probe for a diagnostic line, so a failure report carries the probe that
// produced it rather than only the fact that one exists.
func probeSummary(probe *corev1.Probe) string {
	if probe == nil {
		return "<none>"
	}

	handler := "<non-exec>"
	if probe.Exec != nil {
		handler = strings.Join(probe.Exec.Command, " ")
	}

	return fmt.Sprintf("{period=%ds timeout=%ds failureThreshold=%d cmd=%q}",
		probe.PeriodSeconds, probe.TimeoutSeconds, probe.FailureThreshold, handler)
}

// probeScansProc reports whether an exec probe decides its verdict by reading the process table.
func probeScansProc(probe *corev1.Probe) bool {
	if probe == nil || probe.Exec == nil {
		return false
	}

	return strings.Contains(strings.Join(probe.Exec.Command, " "), "/proc/")
}

// captureAgentProbeBaselines records, for every node running an agent of this test's SBRC, the pod
// the churn must leave in place and the restart count it must leave unchanged.
func captureAgentProbeBaselines() []agentProbeBaseline {
	agentPods, listErr := sbrcAgentPods(sbrparams.SBRCAgentProbeTestName)
	Expect(listErr).ToNot(HaveOccurred(), "Failed to list the agent pods of SBRC %q",
		sbrparams.SBRCAgentProbeTestName)
	Expect(agentPods).ToNot(BeEmpty(), "SBRC %q must have at least one Running agent pod",
		sbrparams.SBRCAgentProbeTestName)
	baselines := make([]agentProbeBaseline, 0, len(agentPods))

	for _, agentPod := range agentPods {
		restarts, restartErr := agentRestartCount(agentPod.Object.Name)
		Expect(restartErr).ToNot(HaveOccurred(),
			"Must read the restart count of agent pod %q", agentPod.Object.Name)
		baseline := agentProbeBaseline{
			nodeName:     agentPod.Object.Spec.NodeName,
			agentPodName: agentPod.Object.Name,
			agentPodUID:  string(agentPod.Object.UID),
			restarts:     restarts,
		}
		probeEvents, eventErr := agentProbeEvents(baseline)
		Expect(eventErr).ToNot(HaveOccurred(),
			"Must list the events of agent pod %q", baseline.agentPodName)
		baseline.probeEvents = probeEvents
		baselines = append(baselines, baseline)
		GinkgoWriter.Printf("Agent baseline: node=%s pod=%s restarts=%d probeEvents=%d\n",
			baseline.nodeName, baseline.agentPodName, restarts, len(probeEvents))
	}

	return baselines
}

// agentProbeEvents returns the kubelet probe complaints against an agent pod, one formatted line
// each.
func agentProbeEvents(baseline agentProbeBaseline) ([]string, error) {
	events, listErr := helpers.ListEventsForObject(context.TODO(), APIClient.K8sClient,
		helpers.InvolvedObjectRef{
			Kind:      "Pod",
			Name:      baseline.agentPodName,
			Namespace: medik8sparams.OperatorNs,
			UID:       baseline.agentPodUID,
		})
	if listErr != nil {
		return nil, listErr
	}

	var probeEvents []string

	for idx := range events {
		if !isProbeFailureEvent(&events[idx]) {
			continue
		}

		probeEvents = append(probeEvents,
			fmt.Sprintf("%s: %s", events[idx].Reason, events[idx].Message))
	}

	return probeEvents, nil
}

// isProbeFailureEvent reports whether a kubelet event is a probe complaint. It matches on the
// event reason rather than searching the message for "probe", because the SBRC this test creates
// has "probe" in its name, so kubelet embeds that word in every message that names an agent pod —
// a bare substring match counts the pod's own Scheduled event as a probe failure.
func isProbeFailureEvent(event *corev1.Event) bool {
	switch event.Reason {
	case sbrparams.UnhealthyEventReason:
		return true
	case sbrparams.KillingEventReason:
		// Killing also covers ordinary shutdowns; only a probe-driven kill names the probe.
		return strings.Contains(event.Message, "probe")
	default:
		return false
	}
}

// baselineForNode returns the baseline recorded for a node, and whether one was recorded at all.
func baselineForNode(baselines []agentProbeBaseline, nodeName string) (agentProbeBaseline, bool) {
	for _, baseline := range baselines {
		if baseline.nodeName == nodeName {
			return baseline, true
		}
	}

	return agentProbeBaseline{}, false
}

// agentsNotRestarted returns nil while every agent is still the pod recorded in its baseline, with
// the restart count it was recorded at. Intended as a Consistently body.
func agentsNotRestarted(baselines []agentProbeBaseline) error {
	for _, baseline := range baselines {
		currentName, nameErr := agentPodNameByNode(sbrparams.SBRCAgentProbeTestName, baseline.nodeName, true)
		if nameErr != nil {
			return fmt.Errorf("agent pod on node %s: %w", baseline.nodeName, nameErr)
		}

		if currentName != baseline.agentPodName {
			return fmt.Errorf("the agent pod on node %s was replaced: %s -> %s",
				baseline.nodeName, baseline.agentPodName, currentName)
		}

		restarts, restartErr := agentRestartCount(currentName)
		if restartErr != nil {
			return restartErr
		}

		if restarts != baseline.restarts {
			return fmt.Errorf("agent pod %s on node %s was restarted: restart count went from %d to %d",
				currentName, baseline.nodeName, baseline.restarts, restarts)
		}
	}

	return nil
}

// targetAgentRestartedWhileUp returns an error only for the signature RHWA-1955 produces: the
// target node is still Ready on the boot it started with, and its agent's container restart count
// has gone up — kubelet killed a healthy agent on a running node. A node that has rebooted, gone
// NotReady or dropped out of the API restarts its agent as a matter of course, and that is the
// outcome the caller is waiting for rather than the bug, so all three read as nil.
func targetAgentRestartedWhileUp(baseline agentProbeBaseline, preFenceBootID string) error {
	node, nodeErr := APIClient.CoreV1Interface.Nodes().Get(
		context.TODO(), baseline.nodeName, metav1.GetOptions{})
	if nodeErr != nil {
		return nil
	}

	if node.Status.NodeInfo.BootID != preFenceBootID || !nodeReportsReady(node) {
		return nil
	}

	agentPod, getErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).Get(
		context.TODO(), baseline.agentPodName, metav1.GetOptions{})
	if getErr != nil {
		// Gone, or unreadable for a moment: either way not evidence of a restart.
		return nil
	}

	var restarts int32

	for _, containerStatus := range agentPod.Status.ContainerStatuses {
		restarts += containerStatus.RestartCount
	}

	if restarts != baseline.restarts {
		return fmt.Errorf("agent pod %s restart count went from %d to %d while node %s was Ready on "+
			"boot-id %s", baseline.agentPodName, baseline.restarts, restarts, baseline.nodeName,
			preFenceBootID)
	}

	return nil
}

// nodeReportsReady reports whether a node currently has Ready=True.
func nodeReportsReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// assertNoAgentProbeEvents asserts the churn added no probe complaint against any agent pod. The
// restart count catches only the third consecutive failure; the events catch the first, so a probe
// that misreads the process table occasionally fails here before it ever kills anything. The
// comparison is against the events each pod already had, so a readiness complaint from a pod that
// was still starting when the test began is not blamed on the churn.
func assertNoAgentProbeEvents(baselines []agentProbeBaseline) {
	for _, baseline := range baselines {
		probeEvents, eventErr := agentProbeEvents(baseline)
		Expect(eventErr).ToNot(HaveOccurred(),
			"Must list the events of agent pod %q", baseline.agentPodName)
		Expect(probeEvents).To(Equal(baseline.probeEvents),
			"kubelet must record no new probe failure against agent pod %q on node %q: the agent was "+
				"healthy throughout the churn (RHWA-1955). Before: %v. After: %v",
			baseline.agentPodName, baseline.nodeName, baseline.probeEvents, probeEvents)
	}
}

// assertWatchdogHeldOnNode asserts some host process holds a watchdog device open on the node. A
// node whose storage is gone reboots itself only because its agent is still holding that device, so
// this is what must remain true for fencing to be able to complete at all.
func assertWatchdogHeldOnNode(nodeName string) {
	phase, podLog := runHostCommandPod(nodeName, sbrparams.AgentProbeWatchdogPodName,
		watchdogHolderScript(), sbrparams.BlockPartitionPodTimeout)
	Expect(phase).To(Equal(corev1.PodSucceeded),
		"The watchdog holder check must run on node %q (ended %s): %s", nodeName, phase, podLog)
	GinkgoWriter.Printf("Watchdog holders on node %q:\n%s\n", nodeName, podLog)
	Expect(podLog).To(ContainSubstring("held by"),
		"Some process on node %q must hold a watchdog device open: without it the node cannot "+
			"reboot itself when it loses the shared storage, however its peers judge it (RHWA-1955)",
		nodeName)
}
