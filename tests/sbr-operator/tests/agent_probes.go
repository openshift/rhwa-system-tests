package tests

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// namedProbe pairs a probe with the slot it occupies, so an assertion message can say which
// probe broke the contract.
type namedProbe struct {
	name  string
	probe *corev1.Probe
}

// getAgentContainer returns the sbr-agent container of a DaemonSet pod template.
func getAgentContainer(agentDS *appsv1.DaemonSet) corev1.Container {
	for _, container := range agentDS.Spec.Template.Spec.Containers {
		if container.Name == sbrparams.AgentContainerName {
			return container
		}
	}

	Fail(fmt.Sprintf("DaemonSet %s must define a container named %q",
		agentDS.Name, sbrparams.AgentContainerName))

	return corev1.Container{}
}

// getAgentDaemonSet fetches the agent DaemonSet of the named SBRC, waiting for the controller
// to create it. Shared storage (PVC bind and SBD device init) is reconciled before the
// DaemonSet, so on a cold storage backend this legitimately takes minutes.
func getAgentDaemonSet(sbrcName string) *appsv1.DaemonSet {
	dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrcName

	var agentDS *appsv1.DaemonSet

	Eventually(func() error {
		fetched, getErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
			context.TODO(), dsName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("DaemonSet %s not created yet: %w", dsName, getErr)
		}

		agentDS = fetched

		return nil
	}, sbrparams.SBRCReadyTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		func() string {
			return fmt.Sprintf("Controller must create agent DaemonSet %s for SBRC %q\n%s",
				dsName, sbrcName, sbrcReadinessDiagnostics(sbrcName, dsName))
		})

	return agentDS
}

// assertNoProcessTableProbe asserts that no probe of the agent container reads the host
// process table. The agent pod sets hostPID, so a probe such as
// "grep -l sbr-agent /proc/*/cmdline" matches the probe's own sh and grep and can never
// detect a dead agent, while exiting 2 whenever any host process disappears mid-scan.
func assertNoProcessTableProbe(container corev1.Container) {
	for _, candidate := range []namedProbe{
		{name: "liveness", probe: container.LivenessProbe},
		{name: "readiness", probe: container.ReadinessProbe},
		{name: "startup", probe: container.StartupProbe},
	} {
		if candidate.probe == nil || candidate.probe.Exec == nil {
			continue
		}

		command := strings.Join(candidate.probe.Exec.Command, " ")

		for _, forbidden := range sbrparams.AgentProbeForbiddenSubstrings {
			Expect(command).ToNot(ContainSubstring(forbidden),
				"The agent %s probe must not scan the host process table, but its command %q contains %q",
				candidate.name, command, forbidden)
		}
	}
}

// assertNoLivenessOrStartupProbe asserts the agent container carries neither probe. Both
// restart the agent, and a restart releases the watchdog: if it lands while the node is cut
// off from shared storage the replacement cannot pass pre-flight, leaving a node its peers
// have already fenced with nothing able to reboot it. The agent is the container ENTRYPOINT,
// so a genuinely dead agent exits the container and kubelet restarts it without any probe.
func assertNoLivenessOrStartupProbe(container corev1.Container, mode string) {
	Expect(container.LivenessProbe).To(BeNil(),
		"The agent container must carry no liveness probe (%s)", mode)
	Expect(container.StartupProbe).To(BeNil(),
		"The agent container must carry no startup probe (%s)", mode)
}

var _ = Describe(
	"SBR Agent - DaemonSet probe contract",
	Ordered,
	Label(
		labels.OperatorSBR,
		labels.TierSmoke,
		labels.FrequencyPresubmit,
		labels.DisruptionNonDestructive,
		labels.PlatformAny,
		labels.ComponentDaemonSet,
	), func() {
		var rwxStorageClass string

		BeforeAll(func() {
			By("Discovering an RWX-capable StorageClass, which the controller requires before " +
				"it renders the agent DaemonSet")

			rwxStorageClass = discoverRWXStorageClass()

			By("Deleting any probe-contract SBRC left behind by a previous run")

			staleRef := buildSBRC(sbrparams.SBRCProbeShapeTestName, map[string]interface{}{})
			deleteErr := APIClient.Delete(context.TODO(), staleRef)
			if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
				GinkgoT().Logf("Warning: pre-test cleanup of stale SBRC %s failed: %v",
					sbrparams.SBRCProbeShapeTestName, deleteErr)
			}

			By("Waiting for the stale SBRC and its DaemonSet to be fully gone")

			Eventually(func() error {
				getErr := APIClient.Get(context.TODO(),
					types.NamespacedName{
						Name:      sbrparams.SBRCProbeShapeTestName,
						Namespace: medik8sparams.OperatorNs,
					},
					buildSBRC(sbrparams.SBRCProbeShapeTestName, map[string]interface{}{}))
				if k8serrors.IsNotFound(getErr) {
					return nil
				}

				if getErr != nil {
					return getErr
				}

				return fmt.Errorf("SBRC %s still terminating", sbrparams.SBRCProbeShapeTestName)
			}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
				"Stale SBRC %s must be fully deleted before the test recreates it",
				sbrparams.SBRCProbeShapeTestName)

			waitForDaemonSetGC(sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCProbeShapeTestName)
		})

		It("Verify the sbr-agent DaemonSet carries no liveness or startup probe and no probe "+
			"scans the host process table",
			reportxml.ID("1965"),
			func() {
				sbrc := buildSBRC(sbrparams.SBRCProbeShapeTestName, map[string]interface{}{
					"sharedStorageClass": rwxStorageClass,
					// No node matches this selector, so the DaemonSet renders the agent pod
					// template in full while scheduling zero agent pods. The probe contract
					// lives in that template, so the test needs no running agent and disturbs
					// no node.
					"nodeSelector": map[string]interface{}{
						sbrparams.SBRCNoMatchSelectorKey: sbrparams.SBRCNoMatchSelectorValue,
					},
				})

				DeferCleanup(func() {
					By("DeferCleanup: deleting the probe-contract SBRC and waiting for its DaemonSet to be GC-d")

					deleteErr := APIClient.Delete(context.TODO(), sbrc.DeepCopy())
					if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
						GinkgoT().Logf("Warning: failed to delete SBRC %s: %v",
							sbrparams.SBRCProbeShapeTestName, deleteErr)

						return
					}

					waitForDaemonSetGC(sbrparams.SBRAgentDaemonSetPrefix + sbrparams.SBRCProbeShapeTestName)
				})

				By("Creating a StorageBasedRemediationConfig whose nodeSelector matches no node")

				Expect(APIClient.Create(context.TODO(), sbrc)).To(Succeed(),
					"StorageBasedRemediationConfig %q must be created successfully",
					sbrparams.SBRCProbeShapeTestName)

				By("Waiting for the controller to render the agent DaemonSet without scheduling agent pods")

				agentDS := getAgentDaemonSet(sbrparams.SBRCProbeShapeTestName)

				// Checked over a window, not once: the DaemonSet is read as soon as it appears,
				// when its status is still zero-valued either way.
				Consistently(func() error {
					fetched, getErr := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
						context.TODO(), agentDS.Name, metav1.GetOptions{})
					if getErr != nil {
						return getErr
					}

					if fetched.Status.DesiredNumberScheduled > 0 {
						return fmt.Errorf("DaemonSet %s schedules %d agent pod(s)",
							fetched.Name, fetched.Status.DesiredNumberScheduled)
					}

					return nil
				}, sbrparams.NoNewDaemonSetCheckDuration, sbrparams.NoNewDaemonSetCheckInterval).Should(Succeed(),
					"DaemonSet %s must schedule no agent pod, so this test stays non-destructive; "+
						"a node carrying the label %s=%s would break that",
					agentDS.Name, sbrparams.SBRCNoMatchSelectorKey, sbrparams.SBRCNoMatchSelectorValue)

				container := getAgentContainer(agentDS)

				By("Asserting the agent container carries no liveness and no startup probe")

				assertNoLivenessOrStartupProbe(container, "remediation mode")

				By("Asserting no agent probe scans the host process table")

				assertNoProcessTableProbe(container)

				By("Asserting the readiness probe checks the pre-flight sentinel and the watchdog device")

				Expect(container.ReadinessProbe).ToNot(BeNil(),
					"The agent container must keep its readiness probe")
				Expect(container.ReadinessProbe.Exec).ToNot(BeNil(),
					"The agent readiness probe must stay an exec probe")
				Expect(container.ReadinessProbe.Exec.Command).To(Equal([]string{
					"/bin/sh", "-c", fmt.Sprintf("test -f %s && test -c %s",
						sbrparams.AgentPreflightSentinelPath, sbrparams.AgentDefaultWatchdogPath),
				}), "The agent readiness probe must test only the pre-flight sentinel and the watchdog device")

				By("Asserting the readiness probe tuning is unchanged")

				Expect(container.ReadinessProbe.InitialDelaySeconds).
					To(Equal(sbrparams.AgentReadinessInitialDelaySeconds),
						"Agent readiness initialDelaySeconds must stay %d",
						sbrparams.AgentReadinessInitialDelaySeconds)
				Expect(container.ReadinessProbe.PeriodSeconds).
					To(Equal(sbrparams.AgentReadinessPeriodSeconds),
						"Agent readiness periodSeconds must stay %d", sbrparams.AgentReadinessPeriodSeconds)
				Expect(container.ReadinessProbe.TimeoutSeconds).
					To(Equal(sbrparams.AgentReadinessTimeoutSeconds),
						"Agent readiness timeoutSeconds must stay %d", sbrparams.AgentReadinessTimeoutSeconds)
				Expect(container.ReadinessProbe.FailureThreshold).
					To(Equal(sbrparams.AgentReadinessFailureThreshold),
						"Agent readiness failureThreshold must stay %d", sbrparams.AgentReadinessFailureThreshold)
				Expect(container.ReadinessProbe.SuccessThreshold).
					To(Equal(sbrparams.AgentReadinessSuccessThreshold),
						"Agent readiness successThreshold must stay %d", sbrparams.AgentReadinessSuccessThreshold)

				By("Asserting the agent pod template still sets hostPID")

				Expect(agentDS.Spec.Template.Spec.HostPID).To(BeTrue(),
					"The agent pod template must keep hostPID, which is why a process-table probe "+
						"self-matches and must never be reintroduced")

				By("Patching the StorageBasedRemediationConfig into detect-only mode")

				detectOnlyDS := patchSBRCSpecField(sbrparams.SBRCProbeShapeTestName,
					"detectOnlyMode", sbrparams.DetectOnlyModeEnabled, agentDS.Generation)
				detectOnlyContainer := getAgentContainer(detectOnlyDS)

				By("Asserting detect-only readiness drops the watchdog check and adds no other probe")

				assertNoLivenessOrStartupProbe(detectOnlyContainer, "detect-only mode")
				assertNoProcessTableProbe(detectOnlyContainer)
				Expect(detectOnlyContainer.ReadinessProbe).ToNot(BeNil(),
					"The agent container must keep its readiness probe in detect-only mode")
				Expect(detectOnlyContainer.ReadinessProbe.Exec.Command).To(Equal([]string{
					"/bin/sh", "-c", fmt.Sprintf("test -f %s", sbrparams.AgentPreflightSentinelPath),
				}), "In detect-only mode the agent readiness probe must test only the pre-flight sentinel, "+
					"because the watchdog is deliberately not armed")
			})
	})
