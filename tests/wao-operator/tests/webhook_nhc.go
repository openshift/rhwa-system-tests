package tests

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/wao-operator/internal/waoparams"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nhcValidatingWebhook = "vnodehealthcheck.kb.io"

	nhcStoredName        = waoparams.WebhookTestPrefix + "-nhc"
	nhcStoredTemplateOne = waoparams.WebhookTestPrefix + "-snr-template-1"
	nhcStoredTemplateTwo = waoparams.WebhookTestPrefix + "-snr-template-2"

	// nhcNoNodeLabel is a node label no node has, so a stored NodeHealthCheck selects no node.
	nhcNoNodeLabel = "wao-webhook-test.medik8s.io/no-node"

	// nhcRemediationStatus is a status with one remediation entry. NHC admission reads the stored
	// status for the update and delete rules; the entry names a node that does not exist.
	nhcRemediationStatus = `{"status":{"unhealthyNodes":[{"name":"` + waoparams.WebhookTestNodeName +
		`","remediations":[{"resource":{"apiVersion":"` + snrAPIGroup + `/v1alpha1","kind":"SelfNodeRemediation",` +
		`"name":"` + waoparams.WebhookTestNodeName + `","namespace":"` + medik8sparams.OperatorNs +
		`"},"started":"2026-01-01T00:00:00Z"}]}]}}`
	nhcNoRemediationStatus = `{"status":{"unhealthyNodes":null}}`

	nhcOngoingRemediation = "prohibited due to running remediation"
)

var _ = Describe(
	"WAO NHC admission",
	Ordered, Serial,
	Label(labels.OperatorWAO, labels.OperatorNHC, labels.ComponentWebhook), func() {
		BeforeAll(func() {
			By("Removing the NodeHealthCheck and the templates of an earlier run")
			Expect(removeStoredNHC(context.Background())).To(Succeed(),
				"A NodeHealthCheck %s from an earlier run must be removable", nhcStoredName)

			for _, name := range []string{nhcStoredTemplateOne, nhcStoredTemplateTwo} {
				Expect(removeObject(context.Background(), newSNRTemplate(name, "Automatic"))).To(Succeed(),
					"A SelfNodeRemediationTemplate %s from an earlier run must be removable", name)
			}
		})

		It("RHWA-9003: serves NHC admission while NHC is Disabled and while it is Enabled",
			reportxml.ID("9003"),
			Label(
				labels.OperatorWAO,
				labels.OperatorNHC,
				labels.TierAcceptance,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()

				operandAdmissionPhases(medik8sparams.WAOComponentNHC,
					func(state string) {
						verifyAdmissionCases(ctx, state, nhcCreateCases()...)
						verifyNHCDuplicateKindWithAnnotatedTemplates(ctx, state)
						verifyNHCRulesDuringRemediation(ctx, state)
					},
					func(state string) {
						verifyAdmissionCases(ctx, state, nhcCreateCases()...)
					}, nil)
			})
	})

// newNHC builds a valid NodeHealthCheck with the stored test name and applies the change. The
// selector matches no node and the object carries a pause request, so it starts no remediation
// if it is ever stored while NHC runs.
func newNHC(change func(spec map[string]interface{})) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"selector": map[string]interface{}{
			"matchLabels": map[string]interface{}{nhcNoNodeLabel: "true"},
		},
		"pauseRequests": []interface{}{waoparams.WebhookTestPrefix},
		"minHealthy":    "51%",
		"unhealthyConditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": "False", "duration": "300s"},
		},
		"remediationTemplate": templateRef(snrTemplateGVK, snrDefaultTemplateName),
	}

	if change != nil {
		change(spec)
	}

	return newObject(nhcGVK, nhcStoredName, "", spec)
}

// nhcEscalation replaces the remediation template of a NodeHealthCheck spec with escalation
// steps.
func nhcEscalation(steps ...interface{}) func(spec map[string]interface{}) {
	return func(spec map[string]interface{}) {
		delete(spec, "remediationTemplate")
		spec["escalatingRemediations"] = steps
	}
}

// nhcStep builds one escalation step.
func nhcStep(gvk schema.GroupVersionKind, name string, order int64, timeout string) map[string]interface{} {
	return map[string]interface{}{
		"remediationTemplate": templateRef(gvk, name),
		"order":               order,
		"timeout":             timeout,
	}
}

// nhcDenied builds a create case that NHC admission must deny.
func nhcDenied(desc, denial string, change func(spec map[string]interface{})) admissionCase {
	return admissionCase{
		desc:    desc,
		request: dryRunCreate(newNHC(change)),
		webhook: nhcValidatingWebhook,
		denials: []string{denial},
	}
}

// nhcCreateCases are the NodeHealthCheck requests that need no stored object.
func nhcCreateCases() []admissionCase {
	return []admissionCase{
		{desc: "Creating a valid NodeHealthCheck with dry-run", request: dryRunCreate(newNHC(nil))},
		{
			desc: "Creating a NodeHealthCheck with escalation steps of two template kinds with dry-run",
			request: dryRunCreate(newNHC(nhcEscalation(
				nhcStep(snrTemplateGVK, "first", 0, "60s"), nhcStep(farTemplateGVK, "second", 1, "60s")))),
		},
		nhcDenied("Creating a NodeHealthCheck with an empty selector with dry-run", "Selector is mandatory",
			func(spec map[string]interface{}) { spec["selector"] = map[string]interface{}{} }),
		nhcDenied("Creating a NodeHealthCheck with an invalid selector operator with dry-run", "Invalid selector",
			func(spec map[string]interface{}) {
				spec["selector"] = map[string]interface{}{"matchExpressions": []interface{}{
					map[string]interface{}{"key": nhcNoNodeLabel, "operator": "Invalid"},
				}}
			}),
		nhcDenied("Creating a NodeHealthCheck with minHealthy and maxUnhealthy with dry-run",
			"minHealthy and maxUnhealthy cannot be specified at the same time",
			func(spec map[string]interface{}) { spec["maxUnhealthy"] = int64(1) }),
		nhcDenied("Creating a NodeHealthCheck without minHealthy and maxUnhealthy with dry-run",
			"one of minHealthy and maxUnhealthy should be specified",
			func(spec map[string]interface{}) { delete(spec, "minHealthy") }),
		nhcDenied("Creating a NodeHealthCheck with a negative minHealthy with dry-run",
			"minHealthy must not be negative",
			func(spec map[string]interface{}) { spec["minHealthy"] = int64(-1) }),
		nhcDenied("Creating a NodeHealthCheck with maxUnhealthy and an MDR template with dry-run",
			"maxUnhealthy can't be used with MachineDeletionRemediationTemplate",
			func(spec map[string]interface{}) {
				delete(spec, "minHealthy")
				spec["maxUnhealthy"] = int64(1)
				spec["remediationTemplate"] = templateRef(mdrTemplateGVK, "any")
			}),
		nhcDenied("Creating a NodeHealthCheck without a remediation with dry-run",
			"Either RemediationTemplate or at least one EscalatingRemediations must be set",
			func(spec map[string]interface{}) { delete(spec, "remediationTemplate") }),
		nhcDenied("Creating a NodeHealthCheck with a template and escalation steps with dry-run",
			"RemediationTemplate and EscalatingRemediations usage is mutual exclusive",
			func(spec map[string]interface{}) {
				spec["escalatingRemediations"] = []interface{}{nhcStep(farTemplateGVK, "first", 0, "60s")}
			}),
		nhcDenied("Creating a NodeHealthCheck with a duplicate escalation order with dry-run",
			"EscalatingRemediation Order must be unique",
			nhcEscalation(nhcStep(snrTemplateGVK, "first", 0, "60s"), nhcStep(farTemplateGVK, "second", 0, "60s"))),
		nhcDenied("Creating a NodeHealthCheck with an escalation timeout of 59 seconds with dry-run",
			"EscalatingRemediation Timeout must be at least one minute",
			nhcEscalation(nhcStep(snrTemplateGVK, "first", 0, "59s"))),
		nhcDenied("Creating a NodeHealthCheck with two escalation steps of the MDR template kind with dry-run",
			"duplicate template kind: "+mdrTemplateGVK.Kind,
			nhcEscalation(nhcStep(mdrTemplateGVK, "first", 0, "60s"), nhcStep(mdrTemplateGVK, "second", 1, "60s"))),
	}
}

// verifyNHCDuplicateKindWithAnnotatedTemplates checks the permitted case of the duplicate kind
// rule. NHC admission lists the templates of the kind with the WAO ServiceAccount and admits the
// request only when each of them has the annotation the SNR template defaulter adds. The two
// templates are stored for the check and removed before it returns.
func verifyNHCDuplicateKindWithAnnotatedTemplates(ctx context.Context, state string) {
	GinkgoHelper()
	templates := []client.Object{
		newSNRTemplate(nhcStoredTemplateOne, "Automatic"), newSNRTemplate(nhcStoredTemplateTwo, "Automatic"),
	}

	DeferCleanup(func() {
		By("DeferCleanup: removing the stored SelfNodeRemediationTemplates")
		removeObjectsInCleanup(templates...)
	})

	By("Storing two SelfNodeRemediationTemplates without the multiple-templates annotation")

	for _, template := range templates {
		Expect(APIClient.Create(ctx, template)).To(Succeed(),
			"Failed to create SelfNodeRemediationTemplate %s", template.GetName())
	}

	verifyAdmissionCases(ctx, state, admissionCase{
		desc: "Creating a NodeHealthCheck with two escalation steps of the SNR template kind with dry-run",
		request: dryRunCreate(newNHC(nhcEscalation(
			nhcStep(snrTemplateGVK, nhcStoredTemplateOne, 0, "60s"),
			nhcStep(snrTemplateGVK, nhcStoredTemplateTwo, 1, "60s")))),
	})

	By("Deleting the stored SelfNodeRemediationTemplates")

	for _, template := range templates {
		Expect(removeObject(ctx, template)).To(Succeed(),
			"SelfNodeRemediationTemplate %s must be gone", template.GetName())
	}
}

// verifyNHCRulesDuringRemediation checks the update and delete rules that read the stored status.
// It stores one NodeHealthCheck while NHC is Disabled, so no controller changes the status the
// spec writes, and removes the object before it returns.
func verifyNHCRulesDuringRemediation(ctx context.Context, state string) {
	GinkgoHelper()
	stored := newNHC(nil)

	DeferCleanup(func() {
		By("DeferCleanup: removing the stored NodeHealthCheck")

		if err := removeStoredNHC(context.Background()); err != nil {
			GinkgoWriter.Printf("DeferCleanup: %v\n", err)
		}
	})

	By("Storing a paused NodeHealthCheck that selects no node")
	Expect(APIClient.Create(ctx, stored.DeepCopy())).To(Succeed(), "Failed to create NodeHealthCheck %s", nhcStoredName)

	By("Writing a remediation entry to the status of the stored NodeHealthCheck")
	Expect(APIClient.Status().Patch(ctx, stored.DeepCopy(),
		client.RawPatch(types.MergePatchType, []byte(nhcRemediationStatus)))).To(Succeed(),
		"Failed to write status.unhealthyNodes of NodeHealthCheck %s", nhcStoredName)

	verifyAdmissionCases(ctx, state,
		admissionCase{
			desc:    "Deleting the stored NodeHealthCheck with dry-run",
			request: dryRunDelete(stored),
			webhook: nhcValidatingWebhook,
			denials: []string{"deletion " + nhcOngoingRemediation},
		},
		admissionCase{
			desc: "Updating the selector of the stored NodeHealthCheck with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, "false", "spec", "selector", "matchLabels", nhcNoNodeLabel)
			}),
			webhook: nhcValidatingWebhook,
			denials: []string{"selector update " + nhcOngoingRemediation},
		},
		admissionCase{
			desc: "Updating the remediation template of the stored NodeHealthCheck with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, "other", "spec", "remediationTemplate", "name")
			}),
			webhook: nhcValidatingWebhook,
			denials: []string{"remediation template update " + nhcOngoingRemediation},
		},
		admissionCase{
			desc: "Updating the pause requests of the stored NodeHealthCheck with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedStringSlice(object.Object,
					[]string{waoparams.WebhookTestPrefix, "second"}, "spec", "pauseRequests")
			}),
		})

	By("Removing the remediation entry and deleting the stored NodeHealthCheck")
	Expect(removeStoredNHC(ctx)).To(Succeed(), "NodeHealthCheck %s must be gone", nhcStoredName)
}

// removeStoredNHC removes the stored NodeHealthCheck. It clears the remediation entry first,
// because NHC admission denies the delete while the entry exists.
func removeStoredNHC(ctx context.Context) error {
	stored := newNHC(nil)
	err := APIClient.Status().Patch(ctx, stored.DeepCopy(),
		client.RawPatch(types.MergePatchType, []byte(nhcNoRemediationStatus)))
	if client.IgnoreNotFound(err) != nil {
		return err
	}

	return removeObject(ctx, stored)
}
