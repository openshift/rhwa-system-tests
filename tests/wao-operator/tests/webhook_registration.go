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

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// registeredWebhook is one webhook entry as the API server holds it, reduced to the fields the
// registration spec compares.
type registeredWebhook struct {
	waoparams.WebhookEntry

	configuration string
	serviceNs     string
	serviceName   string
	hasCABundle   bool
	sideEffects   string
	rules         int
}

// servedByWAO reports whether the entry calls the WAO admission Service.
func (webhook registeredWebhook) servedByWAO() bool {
	return webhook.serviceNs == medik8sparams.OperatorNs && webhook.serviceName == waoparams.WebhookServiceName
}

var _ = Describe(
	"WAO admission webhook registration",
	Ordered,
	Label(labels.OperatorWAO, labels.ComponentWebhook), func() {
		BeforeAll(func() {
			By("Verifying the WorkloadAvailabilityConfig singleton exists")

			_, err := helpers.GetWAOConfig(context.Background(), APIClient)
			Expect(err).ToNot(HaveOccurred(), "WAO is not installed on this cluster")
		})

		It("RHWA-9001: registers every admission entry and routes it to the WAO Service",
			reportxml.ID("9001"),
			Label(
				labels.OperatorWAO,
				labels.TierAcceptance,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()

				By("Listing every webhook entry on the cluster")

				registered, err := listRegisteredWebhooks(ctx)
				Expect(err).ToNot(HaveOccurred(), "Failed to list the webhook configurations")

				By("Verifying each expected entry calls the WAO Service with its failurePolicy, operations and timeout")
				Expect(missingOrDifferentEntries(registered)).To(BeEmpty(),
					"Each entry of waoparams.WebhookEntries must be registered as listed")

				By("Verifying the WAO Service serves no entry outside the expected list")
				Expect(unexpectedWAOEntries(registered)).To(BeEmpty(),
					"The WAO Service must serve only the entries of waoparams.WebhookEntries")

				By("Verifying each WAO entry has a CA bundle, one rule and no side effects")
				Expect(malformedWAOEntries(registered)).To(BeEmpty(),
					"Each WAO entry needs a CA bundle, exactly one rule and sideEffects None")

				By("Verifying no other Service intercepts a resource that WAO serves")
				Expect(foreignEntriesForWAOResources(registered)).To(BeEmpty(),
					"Only the WAO Service may intercept the resources of the central admission operands")

				By("Verifying the WAO Service has one ready endpoint for each WAO manager replica")
				Eventually(func() error {
					return webhookEndpointsMatchReplicas(ctx)
				}, waoparams.OperandAvailableTimeout, waoparams.DefaultPollInterval).Should(Succeed(),
					"Service %s must have one ready endpoint for each replica of Deployment %s",
					waoparams.WebhookServiceName, waoparams.WAODeploymentName)

				for _, operand := range waoparams.AdmissionOperands {
					expectAdmissionReady(operand)
				}
			})
	})

// listRegisteredWebhooks reads every entry of every validating and mutating webhook
// configuration.
func listRegisteredWebhooks(ctx context.Context) ([]registeredWebhook, error) {
	var registered []registeredWebhook

	validating := &admissionregistrationv1.ValidatingWebhookConfigurationList{}
	if err := APIClient.List(ctx, validating); err != nil {
		return nil, fmt.Errorf("list ValidatingWebhookConfigurations: %w", err)
	}

	for cfgIdx := range validating.Items {
		for _, webhook := range validating.Items[cfgIdx].Webhooks {
			registered = append(registered, newRegisteredWebhook(validating.Items[cfgIdx].Name, false, webhook.Name,
				webhook.ClientConfig, webhook.Rules, webhook.FailurePolicy, webhook.SideEffects, webhook.TimeoutSeconds))
		}
	}

	mutating := &admissionregistrationv1.MutatingWebhookConfigurationList{}
	if err := APIClient.List(ctx, mutating); err != nil {
		return nil, fmt.Errorf("list MutatingWebhookConfigurations: %w", err)
	}

	for cfgIdx := range mutating.Items {
		for _, webhook := range mutating.Items[cfgIdx].Webhooks {
			registered = append(registered, newRegisteredWebhook(mutating.Items[cfgIdx].Name, true, webhook.Name,
				webhook.ClientConfig, webhook.Rules, webhook.FailurePolicy, webhook.SideEffects, webhook.TimeoutSeconds))
		}
	}

	return registered, nil
}

// newRegisteredWebhook reduces one entry to the compared fields. Group, Resource and Operations
// come from the first rule; rules holds the rule count so that a second rule is visible.
func newRegisteredWebhook(
	configuration string, mutating bool, name string,
	clientConfig admissionregistrationv1.WebhookClientConfig,
	rules []admissionregistrationv1.RuleWithOperations,
	failurePolicy *admissionregistrationv1.FailurePolicyType,
	sideEffects *admissionregistrationv1.SideEffectClass,
	timeoutSeconds *int32,
) registeredWebhook {
	webhook := registeredWebhook{
		configuration: configuration, hasCABundle: len(clientConfig.CABundle) > 0, rules: len(rules),
	}
	webhook.Name = name
	webhook.Mutating = mutating

	if clientConfig.Service != nil {
		webhook.serviceNs = clientConfig.Service.Namespace
		webhook.serviceName = clientConfig.Service.Name
	}

	if len(rules) > 0 {
		webhook.Group = strings.Join(rules[0].APIGroups, ",")
		webhook.Resource = strings.Join(rules[0].Resources, ",")

		for _, operation := range rules[0].Operations {
			webhook.Operations = append(webhook.Operations, string(operation))
		}
	}

	if failurePolicy != nil {
		webhook.FailurePolicy = string(*failurePolicy)
	}

	if sideEffects != nil {
		webhook.sideEffects = string(*sideEffects)
	}

	if timeoutSeconds != nil {
		webhook.TimeoutSeconds = *timeoutSeconds
	}

	return webhook
}

// missingOrDifferentEntries names each expected entry that the WAO Service does not serve
// exactly once with the listed type, rule, failurePolicy and timeout.
func missingOrDifferentEntries(registered []registeredWebhook) []string {
	var problems []string

	for _, want := range waoparams.WebhookEntries {
		var found []registeredWebhook

		for _, webhook := range registered {
			if webhook.Name == want.Name && webhook.servedByWAO() {
				found = append(found, webhook)
			}
		}

		if len(found) != 1 {
			problems = append(problems, fmt.Sprintf("%s: the WAO Service serves %d entries, want 1", want.Name, len(found)))

			continue
		}

		got := found[0].WebhookEntry
		if got.Mutating != want.Mutating || got.Group != want.Group || got.Resource != want.Resource ||
			!slices.Equal(got.Operations, want.Operations) || got.FailurePolicy != want.FailurePolicy ||
			got.TimeoutSeconds != want.TimeoutSeconds {
			problems = append(problems, fmt.Sprintf("%s: registered as %+v, want %+v", want.Name, got, want))
		}
	}

	return problems
}

// unexpectedWAOEntries names each entry the WAO Service serves that is not an expected one.
func unexpectedWAOEntries(registered []registeredWebhook) []string {
	var problems []string

	for _, webhook := range registered {
		if !webhook.servedByWAO() {
			continue
		}

		expected := slices.ContainsFunc(waoparams.WebhookEntries, func(want waoparams.WebhookEntry) bool {
			return want.Name == webhook.Name
		})
		if !expected {
			problems = append(problems, fmt.Sprintf("%s in %s", webhook.Name, webhook.configuration))
		}
	}

	return problems
}

// malformedWAOEntries names each WAO entry without a CA bundle, with more or less than one rule,
// or with side effects.
func malformedWAOEntries(registered []registeredWebhook) []string {
	var problems []string

	for _, webhook := range registered {
		if !webhook.servedByWAO() {
			continue
		}

		if !webhook.hasCABundle || webhook.rules != 1 || webhook.sideEffects != "None" {
			problems = append(problems, fmt.Sprintf("%s in %s: caBundle present %v, %d rules, sideEffects %q",
				webhook.Name, webhook.configuration, webhook.hasCABundle, webhook.rules, webhook.sideEffects))
		}
	}

	return problems
}

// foreignEntriesForWAOResources names each entry of a different Service that intercepts a
// resource WAO serves, for example a webhook an operand still registers for itself.
func foreignEntriesForWAOResources(registered []registeredWebhook) []string {
	var problems []string

	for _, webhook := range registered {
		if webhook.servedByWAO() {
			continue
		}

		served := slices.ContainsFunc(waoparams.WebhookEntries, func(want waoparams.WebhookEntry) bool {
			return want.Group == webhook.Group && want.Resource == webhook.Resource
		})
		if served {
			problems = append(problems, fmt.Sprintf("%s in %s calls Service %s/%s for %s/%s",
				webhook.Name, webhook.configuration, webhook.serviceNs, webhook.serviceName,
				webhook.Group, webhook.Resource))
		}
	}

	return problems
}

// webhookEndpointsMatchReplicas compares the ready endpoints of the WAO admission Service with
// the replicas of the WAO manager Deployment.
func webhookEndpointsMatchReplicas(ctx context.Context) error {
	manager := &appsv1.Deployment{}
	err := APIClient.Get(ctx,
		client.ObjectKey{Namespace: medik8sparams.OperatorNs, Name: waoparams.WAODeploymentName}, manager)
	if err != nil {
		return fmt.Errorf("read Deployment %s: %w", waoparams.WAODeploymentName, err)
	}

	slicesList := &discoveryv1.EndpointSliceList{}
	err = APIClient.List(ctx, slicesList, client.InNamespace(medik8sparams.OperatorNs),
		client.MatchingLabels{discoveryv1.LabelServiceName: waoparams.WebhookServiceName})
	if err != nil {
		return fmt.Errorf("list EndpointSlices of Service %s: %w", waoparams.WebhookServiceName, err)
	}

	ready := int32(0)

	for sliceIdx := range slicesList.Items {
		for _, endpoint := range slicesList.Items[sliceIdx].Endpoints {
			if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
				ready++
			}
		}
	}

	want := int32(1)
	if manager.Spec.Replicas != nil {
		want = *manager.Spec.Replicas
	}

	if ready != want || manager.Status.ReadyReplicas != want {
		return fmt.Errorf("%d ready endpoints and %d ready replicas, want %d of each",
			ready, manager.Status.ReadyReplicas, want)
	}

	return nil
}
