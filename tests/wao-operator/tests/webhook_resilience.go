package tests

import (
	"context"
	"fmt"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/wao-operator/internal/waoparams"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// failedCallingWebhook is how the API server reports a webhook that did not answer.
const failedCallingWebhook = "failed calling webhook"

var _ = Describe(
	"WAO admission resilience",
	Ordered, Serial,
	Label(labels.OperatorWAO, labels.ComponentWebhook), func() {
		BeforeAll(func() {
			By("Verifying the WorkloadAvailabilityConfig singleton exists")

			_, err := helpers.GetWAOConfig(context.Background(), APIClient)
			Expect(err).ToNot(HaveOccurred(), "WAO is not installed on this cluster")

			By("Removing the NetworkPolicy of an earlier run")
			Expect(removeObject(context.Background(), newAdmissionOutagePolicy())).To(Succeed(),
				"NetworkPolicy %s from an earlier run must be removable", waoparams.AdmissionOutagePolicyName)
		})

		It("RHWA-9006: obeys the failurePolicy of each entry while the admission listener does not answer",
			reportxml.ID("9006"),
			Label(
				labels.OperatorWAO,
				labels.TierResiliency,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()
				probes := probeCases(ctx)
				policy := newAdmissionOutagePolicy()

				DeferCleanup(func() {
					By("DeferCleanup: removing the NetworkPolicy that isolates the admission listener")
					removeObjectsInCleanup(policy)
				})

				verifyAdmissionCases(ctx, "before the outage", probes...)

				By("Creating a NetworkPolicy that denies ingress to the WAO manager Pods")
				Expect(APIClient.Create(ctx, policy.DeepCopy())).To(Succeed(),
					"Failed to create NetworkPolicy %s", waoparams.AdmissionOutagePolicyName)

				By("Waiting for the API server to lose the admission listener")
				Eventually(func() error {
					_, err := probes[0].request(ctx, webhookClient())
					if err == nil {
						return fmt.Errorf("admission still admits the request")
					}

					if !strings.Contains(err.Error(), failedCallingWebhook) {
						return fmt.Errorf("admission still answers: %w", err)
					}

					return nil
				}, waoparams.AdmissionOutageTimeout, waoparams.DefaultPollInterval).Should(Succeed(),
					"A request must fail with %q once NetworkPolicy %s isolates the listener",
					failedCallingWebhook, waoparams.AdmissionOutagePolicyName)

				verifyAdmissionCases(ctx, "listener isolated", outageCases(probes)...)

				By("Verifying the WAO manager Pods are still Ready")
				Expect(webhookEndpointsMatchReplicas(ctx)).To(Succeed(),
					"The NetworkPolicy must not make a WAO manager Pod unready")

				By("Deleting the NetworkPolicy")
				Expect(removeObject(ctx, policy)).To(Succeed(),
					"NetworkPolicy %s must be gone", waoparams.AdmissionOutagePolicyName)
				waitForAdmission(ctx, "after the outage", probes)
			})

		It("RHWA-9007: serves admission again after every WAO manager Pod restarts",
			reportxml.ID("9007"),
			Label(
				labels.OperatorWAO,
				labels.TierResiliency,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()
				probes := probeCases(ctx)
				verifyAdmissionCases(ctx, "before the restart", probes...)

				By("Deleting every WAO manager Pod")

				before, err := managerPodNames(ctx)
				Expect(err).ToNot(HaveOccurred(), "Failed to list the WAO manager Pods")
				Expect(before).ToNot(BeEmpty(), "No WAO manager Pod found in %s", medik8sparams.OperatorNs)
				Expect(APIClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(medik8sparams.OperatorNs),
					client.MatchingLabels(waoparams.ManagerPodLabels))).To(Succeed(),
					"Failed to delete the WAO manager Pods")

				By("Waiting for new WAO manager Pods to become Ready")
				Eventually(func() error {
					current, listErr := managerPodNames(ctx)
					if listErr != nil {
						return listErr
					}

					for _, name := range current {
						if slices.Contains(before, name) {
							return fmt.Errorf("pod %s from before the restart still exists", name)
						}
					}

					return webhookEndpointsMatchReplicas(ctx)
				}, waoparams.AdmissionRecoveryTimeout, waoparams.DefaultPollInterval).Should(Succeed(),
					"Deployment %s must replace every Pod and report them Ready", waoparams.WAODeploymentName)

				for _, operand := range waoparams.AdmissionOperands {
					expectAdmissionReady(operand)
				}

				waitForAdmission(ctx, "after the restart", probes)

				// A new Pod reads the fence agent inventory from the FAR image before its manager
				// starts, so the agent rule shows the inventory was collected again.
				verifyAdmissionCases(ctx, "after the restart", admissionCase{
					desc:    "Creating a FenceAgentsRemediation with an agent that is not in the FAR image with dry-run",
					request: dryRunCreate(newFAR(farSpec(farField("agent", farUnsupportedAgent)))),
					webhook: farValidatingWebhook,
					denials: []string{"unsupported fence agent: " + farUnsupportedAgent},
				})
			})
	})

// newAdmissionOutagePolicy builds a NetworkPolicy that denies all ingress to the WAO manager
// Pods. The API server then gets no answer from the admission listener. The kubelet probes come
// from the node of the Pod, which a NetworkPolicy does not block, so the Pods stay Ready.
func newAdmissionOutagePolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      waoparams.AdmissionOutagePolicyName,
			Namespace: medik8sparams.OperatorNs,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: waoparams.ManagerPodLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
}

// outageCases turns the valid probe requests into the answers a missing listener must give: a
// failed call for an entry with failurePolicy Fail, and an admitted request for the one entry
// with Ignore. For a kind with a mutating entry the API server calls that entry first, so it is
// the one the failure names.
func outageCases(probes []admissionCase) []admissionCase {
	firstWebhook := []string{
		"mfenceagentsremediation.kb.io",
		"mfenceagentsremediationtemplate.kb.io",
		nhcValidatingWebhook,
		nmoValidatingWebhook,
		snrConfigValidatingWebhook,
		"mselfnoderemediationtemplate.kb.io",
		"", // vselfnoderemediation.kb.io has failurePolicy Ignore: the request is admitted.
	}
	cases := make([]admissionCase, 0, len(probes))

	for idx, probe := range probes {
		if firstWebhook[idx] != "" {
			probe.webhook = firstWebhook[idx]
			probe.callFails = true
		}

		cases = append(cases, probe)
	}

	return cases
}

// waitForAdmission waits until every probe request is admitted again, then sends them one more
// time as the recorded steps.
func waitForAdmission(ctx context.Context, state string, probes []admissionCase) {
	GinkgoHelper()

	By(fmt.Sprintf("Waiting for admission to answer again [%s]", state))
	Eventually(func() []string {
		var failures []string

		for _, probe := range probes {
			admitted, err := probe.request(ctx, webhookClient())
			if failure := probe.evaluate(admitted, err, nil); failure != "" {
				failures = append(failures, probe.desc+": "+failure)
			}
		}

		return failures
	}, waoparams.AdmissionRecoveryTimeout, waoparams.DefaultPollInterval).Should(BeEmpty(),
		"Every kind must be admitted again %s", state)

	verifyAdmissionCases(ctx, state, probes...)
}

// managerPodNames lists the WAO manager Pods.
func managerPodNames(ctx context.Context) ([]string, error) {
	pods := &corev1.PodList{}
	err := APIClient.List(ctx, pods, client.InNamespace(medik8sparams.OperatorNs),
		client.MatchingLabels(waoparams.ManagerPodLabels))
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(pods.Items))
	for podIdx := range pods.Items {
		names = append(names, pods.Items[podIdx].Name)
	}

	return names, nil
}
