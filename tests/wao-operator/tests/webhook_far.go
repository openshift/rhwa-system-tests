package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/wao-operator/internal/waoparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	farValidatingWebhook         = "vfenceagentsremediation.kb.io"
	farTemplateValidatingWebhook = "vfenceagentsremediationtemplate.kb.io"

	// farLegacySecretName is the Secret name FAR admission defaults to when the Secret exists.
	farLegacySecretName = "fence-agents-credentials-shared"

	// farSecretParameter is the one parameter the test Secret defines.
	farSecretParameter = "--password"

	farStoredTemplateName = waoparams.WebhookTestPrefix + "-far-template"
	farUnsupportedAgent   = "fence_incorrect"
)

var _ = Describe(
	"WAO FAR admission",
	Ordered, Serial,
	Label(labels.OperatorWAO, labels.OperatorFAR, labels.ComponentWebhook), func() {
		BeforeAll(func() {
			By("Verifying the legacy FAR Secret and the stored test template do not exist")
			Expect(removeObject(context.Background(), newFARTemplate(farStoredTemplateName, farSpec()))).To(Succeed(),
				"A FenceAgentsRemediationTemplate %s from an earlier run must be removable", farStoredTemplateName)
			Expect(removeStaleLegacySecret(context.Background())).To(Succeed(),
				"Secret %s must be absent, or be a leftover of this spec", farLegacySecretName)
		})

		It("RHWA-9002: serves FAR admission while FAR is Disabled and while it is Enabled",
			reportxml.ID("9002"),
			Label(
				labels.OperatorWAO,
				labels.OperatorFAR,
				labels.TierAcceptance,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()

				operandAdmissionPhases(medik8sparams.WAOComponentFAR,
					func(state string) {
						verifyAdmissionCases(ctx, state, farCreateCases()...)
						verifyFARSecretRules(ctx, state)
					},
					func(state string) {
						verifyAdmissionCases(ctx, state, farCreateCases()...)
					}, nil)
			})
	})

// farSpec builds a valid FenceAgentsRemediation spec and applies the changes. The spec carries no
// credential and names no real node.
func farSpec(changes ...func(spec map[string]interface{})) map[string]interface{} {
	spec := map[string]interface{}{
		"agent": "fence_ipmilan",
		"sharedparameters": map[string]interface{}{
			"--action":  "reboot",
			"--ip":      "192.0.2.1",
			"--lanplus": "",
		},
		"nodeparameters": map[string]interface{}{
			"--ipport": map[string]interface{}{waoparams.WebhookTestNodeName: "6233"},
		},
		"retrycount":          int64(5),
		"retryinterval":       "20s",
		"timeout":             "60s",
		"remediationStrategy": "ResourceDeletion",
	}

	for _, change := range changes {
		change(spec)
	}

	return spec
}

// farSharedParameter sets one shared parameter of a FenceAgentsRemediation spec.
func farSharedParameter(name, value string) func(spec map[string]interface{}) {
	return func(spec map[string]interface{}) {
		shared, isMap := spec["sharedparameters"].(map[string]interface{})
		if isMap {
			shared[name] = value
		}
	}
}

// farField sets one top-level field of a FenceAgentsRemediation spec; a nil value removes it.
func farField(name string, value interface{}) func(spec map[string]interface{}) {
	return func(spec map[string]interface{}) {
		if value == nil {
			delete(spec, name)

			return
		}

		spec[name] = value
	}
}

// newFAR builds a remediation request for a node that does not exist. The specs send it with
// dry-run only.
func newFAR(spec map[string]interface{}) *unstructured.Unstructured {
	return newObject(farGVK, waoparams.WebhookTestNodeName, medik8sparams.OperatorNs, spec)
}

// newFARTemplate builds a FenceAgentsRemediationTemplate around a remediation spec.
func newFARTemplate(name string, spec map[string]interface{}) *unstructured.Unstructured {
	return newObject(farTemplateGVK, name, medik8sparams.OperatorNs, map[string]interface{}{
		"template": map[string]interface{}{"spec": spec},
	})
}

// farCreateCases are the FAR requests that need no stored object.
func farCreateCases() []admissionCase {
	templateName := waoparams.WebhookTestPrefix + "-far-dry-run"
	missingSecret := waoparams.WebhookTestPrefix + "-missing-secret"

	return []admissionCase{
		{desc: "Creating a valid FenceAgentsRemediation with dry-run", request: dryRunCreate(newFAR(farSpec()))},
		{
			desc:    "Creating a FenceAgentsRemediation with the action status with dry-run",
			request: dryRunCreate(newFAR(farSpec(farSharedParameter("--action", "status")))),
			webhook: farValidatingWebhook,
			denials: []string{"FAR doesn't support any other action than"},
		},
		{
			desc:    "Creating a FenceAgentsRemediation with an agent that is not in the FAR image with dry-run",
			request: dryRunCreate(newFAR(farSpec(farField("agent", farUnsupportedAgent)))),
			webhook: farValidatingWebhook,
			denials: []string{"unsupported fence agent: " + farUnsupportedAgent},
		},
		{
			desc:    "Creating a FenceAgentsRemediation with shared parameters only with dry-run",
			request: dryRunCreate(newFAR(farSpec(farField("nodeparameters", nil)))),
			webhook: farValidatingWebhook,
			denials: []string{"mandatory parameters are missing"},
		},
		{
			desc:    "Creating a FenceAgentsRemediation that names a Secret that does not exist with dry-run",
			request: dryRunCreate(newFAR(farSpec(farField("sharedSecretName", missingSecret)))),
			webhook: farValidatingWebhook,
			denials: []string{missingSecret, "not found"},
		},
		{
			desc:    "Creating a valid FenceAgentsRemediationTemplate with dry-run",
			request: dryRunCreate(newFARTemplate(templateName, farSpec())),
			verify:  expectMultipleTemplatesAnnotation("true"),
		},
		{
			desc: "Creating a FenceAgentsRemediationTemplate that sets the multiple-templates annotation with dry-run",
			request: dryRunCreate(withAnnotation(
				newFARTemplate(templateName, farSpec()), multipleTemplatesAnnotation, "false")),
			verify: expectMultipleTemplatesAnnotation("false"),
		},
		{
			desc:    "Creating a FenceAgentsRemediationTemplate with the action status with dry-run",
			request: dryRunCreate(newFARTemplate(templateName, farSpec(farSharedParameter("--action", "status")))),
			webhook: farTemplateValidatingWebhook,
			denials: []string{"FAR doesn't support any other action than"},
		},
		{
			desc: "Creating a FenceAgentsRemediationTemplate with an agent that is not in the FAR image with dry-run",
			request: dryRunCreate(
				newFARTemplate(templateName, farSpec(farField("agent", farUnsupportedAgent)))),
			webhook: farTemplateValidatingWebhook,
			denials: []string{"unsupported fence agent: " + farUnsupportedAgent},
		},
	}
}

// verifyFARSecretRules checks the rules that read a Secret. It stores a Secret with the legacy
// name and one template, and removes both before it returns.
func verifyFARSecretRules(ctx context.Context, state string) {
	GinkgoHelper()
	secret := newLegacySecret()
	stored := newFARTemplate(farStoredTemplateName, farSpec())

	DeferCleanup(func() {
		By("DeferCleanup: removing the stored FenceAgentsRemediationTemplate and the legacy Secret")
		removeObjectsInCleanup(stored, secret)
	})

	By("Storing a Secret with the legacy FAR Secret name")
	Expect(APIClient.Create(ctx, secret)).To(Succeed(), "Failed to create Secret %s", farLegacySecretName)
	verifyAdmissionCases(ctx, state,
		admissionCase{
			desc:    "Creating a FenceAgentsRemediationTemplate without a Secret name with dry-run",
			request: dryRunCreate(newFARTemplate(waoparams.WebhookTestPrefix+"-far-dry-run", farSpec())),
			verify:  expectFARTemplateSecretName(farLegacySecretName),
		},
		admissionCase{
			desc: "Creating a FenceAgentsRemediationTemplate with a parameter that the Secret also has with dry-run",
			request: dryRunCreate(newFARTemplate(waoparams.WebhookTestPrefix+"-far-dry-run",
				farSpec(farSharedParameter(farSecretParameter, "from-the-template")))),
			webhook: farTemplateValidatingWebhook,
			denials: []string{"invalid multiple definition of FAR parameter", farSecretParameter},
		})

	By("Storing a FenceAgentsRemediationTemplate without a Secret name")
	Expect(APIClient.Create(ctx, stored.DeepCopy())).To(Succeed(),
		"Failed to create FenceAgentsRemediationTemplate %s", farStoredTemplateName)

	verifyAdmissionCases(ctx, state,
		admissionCase{
			desc: "Updating the stored FenceAgentsRemediationTemplate to remove the legacy Secret name with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				unstructured.RemoveNestedField(object.Object, "spec", "template", "spec", "sharedSecretName")
			}),
			webhook: farTemplateValidatingWebhook,
			denials: []string{"shared secret with the deprecated default name"},
		},
		admissionCase{
			desc: "Updating the stored FenceAgentsRemediationTemplate to the action status with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, "status",
					"spec", "template", "spec", "sharedparameters", "--action")
			}),
			webhook: farTemplateValidatingWebhook,
			denials: []string{"FAR doesn't support any other action than"},
		},
		admissionCase{
			desc: "Updating the retry count of the stored FenceAgentsRemediationTemplate with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, int64(3), "spec", "template", "spec", "retrycount")
			}),
			verify: expectFARTemplateSecretName(farLegacySecretName),
		})

	By("Deleting the stored FenceAgentsRemediationTemplate and the legacy Secret")
	Expect(removeObject(ctx, stored)).To(Succeed(),
		"FenceAgentsRemediationTemplate %s must be gone", farStoredTemplateName)
	Expect(removeObject(ctx, secret)).To(Succeed(), "Secret %s must be gone", farLegacySecretName)
}

// expectFARTemplateSecretName returns a verify function for the shared Secret name of a template.
func expectFARTemplateSecretName(want string) func(admitted *unstructured.Unstructured) error {
	return func(admitted *unstructured.Unstructured) error {
		got, _, err := unstructured.NestedString(admitted.Object, "spec", "template", "spec", "sharedSecretName")
		if err != nil {
			return fmt.Errorf("read spec.template.spec.sharedSecretName: %w", err)
		}

		if got != want {
			return fmt.Errorf("spec.template.spec.sharedSecretName is %q, want %q", got, want)
		}

		return nil
	}
}

// newLegacySecret builds the Secret with the legacy FAR name. The value is a fixed test string,
// not a credential, and the label marks the Secret as an object of this spec.
func newLegacySecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      farLegacySecretName,
			Namespace: medik8sparams.OperatorNs,
			Labels:    map[string]string{waoparams.WebhookTestPrefix: "true"},
		},
		StringData: map[string]string{farSecretParameter: waoparams.WebhookTestPrefix},
	}
}

// removeStaleLegacySecret removes a legacy Secret an earlier run of this spec left behind. A
// Secret without the label of this spec belongs to somebody else and is an error: the spec must
// not change it, and with it present the Secret rules cannot be checked.
func removeStaleLegacySecret(ctx context.Context) error {
	secret := &corev1.Secret{}
	err := APIClient.Get(ctx, client.ObjectKey{Namespace: medik8sparams.OperatorNs, Name: farLegacySecretName}, secret)
	if k8serrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	if secret.Labels[waoparams.WebhookTestPrefix] != "true" {
		return fmt.Errorf("secret %s exists and this spec did not create it", farLegacySecretName)
	}

	return removeObject(ctx, secret)
}
