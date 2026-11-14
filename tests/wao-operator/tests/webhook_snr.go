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

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	snrConfigValidatingWebhook = "vselfnoderemediationconfig.kb.io"

	snrDryRunTemplateName = waoparams.WebhookTestPrefix + "-snr-dry-run"

	snrPeerTimeoutWarning   = "PeerRequestTimeout (5s) is less than ApiServerTimeout + MinimumBuffer"
	snrConfigDeletedWarning = "The default configuration is deleted, Self Node Remediation is now disabled"
)

var _ = Describe(
	"WAO SNR admission",
	Ordered, Serial,
	Label(labels.OperatorWAO, labels.OperatorSNR, labels.ComponentWebhook), func() {
		It("RHWA-9005: serves SNR admission while SNR is Disabled and while it is Enabled",
			reportxml.ID("9005"),
			Label(
				labels.OperatorWAO,
				labels.OperatorSNR,
				labels.TierAcceptance,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()

				By("Recording which default SNR objects exist")

				created := []*unstructured.Unstructured{}

				for _, object := range []*unstructured.Unstructured{
					newSNRConfig(snrDefaultConfigName, medik8sparams.OperatorNs, nil),
					newSNRTemplate(snrDefaultTemplateName, "Automatic"),
				} {
					if k8serrors.IsNotFound(APIClient.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopy())) {
						created = append(created, object)
					}
				}

				operandAdmissionPhases(medik8sparams.WAOComponentSNR,
					func(state string) {
						verifyAdmissionCases(ctx, state, snrCreateCases()...)
					},
					func(state string) {
						verifyAdmissionCases(ctx, state, snrCreateCases()...)
						verifySNRRulesWithStoredDefaults(ctx, state)
					}, nil)

				if !operandIsEnabled(medik8sparams.WAOComponentSNR) {
					By("Deleting the default SNR objects that the SNR manager created")

					for _, object := range created {
						Expect(removeObject(ctx, object)).To(Succeed(),
							"%s %s must be gone, so that SNR leaves nothing on the cluster",
							object.GetKind(), object.GetName())
					}

					By("Waiting for operand selfNodeRemediation to report phase Disabled")
					Eventually(func() (string, error) {
						return operandPhase(medik8sparams.WAOComponentSNR)
					}, waoparams.OperandDisabledTimeout, waoparams.DefaultPollInterval).
						Should(Equal(medik8sparams.WAOPhaseDisabled),
							"Operand %s must return to phase Disabled when nothing is retained",
							medik8sparams.WAOComponentSNR)
				}
			})
	})

// newSNRConfig builds a SelfNodeRemediationConfig. The specs send it with dry-run only: a stored
// default config starts the agent DaemonSet when SNR is enabled.
func newSNRConfig(name, namespace string, spec map[string]interface{}) *unstructured.Unstructured {
	if spec == nil {
		spec = map[string]interface{}{}
	}

	return newObject(snrConfigGVK, name, namespace, spec)
}

// newSNRTemplate builds a SelfNodeRemediationTemplate with one remediation strategy.
func newSNRTemplate(name, strategy string) *unstructured.Unstructured {
	return newObject(snrTemplateGVK, name, medik8sparams.OperatorNs, map[string]interface{}{
		"template": map[string]interface{}{
			"spec": map[string]interface{}{"remediationStrategy": strategy},
		},
	})
}

// newSNR builds a remediation request for a node that does not exist. The specs send it with
// dry-run only.
func newSNR(strategy string) *unstructured.Unstructured {
	return newObject(snrGVK, waoparams.WebhookTestNodeName, medik8sparams.OperatorNs,
		map[string]interface{}{"remediationStrategy": strategy})
}

// snrConfigDenied builds a create case for the default config name that SNR admission must deny.
func snrConfigDenied(desc string, spec map[string]interface{}, denials ...string) admissionCase {
	return admissionCase{
		desc:    desc,
		request: dryRunCreate(newSNRConfig(snrDefaultConfigName, medik8sparams.OperatorNs, spec)),
		webhook: snrConfigValidatingWebhook,
		denials: denials,
	}
}

// snrTolerations builds a config spec with one custom toleration.
func snrTolerations(toleration map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"customDsTolerations": []interface{}{toleration}}
}

// snrNodeSelector builds a config spec with one custom node selector requirement.
func snrNodeSelector(requirement map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"customDsNodeSelectorRequirements": []interface{}{requirement}}
}

// snrShortDurations is a config spec with each duration below its minimum.
func snrShortDurations() map[string]interface{} {
	return map[string]interface{}{
		"apiServerTimeout":          "0s",
		"apiCheckInterval":          "0.2ms",
		"peerApiServerTimeout":      "0.001s",
		"peerDialTimeout":           "0.009s",
		"peerRequestTimeout":        "0.003ms",
		"peerUpdateInterval":        "0ms",
		"maxTimeForNoPeersResponse": "1ms",
	}
}

// snrShortDurationDenials are the lines SNR admission returns for snrShortDurations.
var snrShortDurationDenials = []string{
	"PeerApiServerTimeout cannot be less than 10ms",
	"ApiServerTimeout cannot be less than 10ms",
	"PeerDialTimeout cannot be less than 10ms",
	"PeerRequestTimeout cannot be less than 10ms",
	"ApiCheckInterval cannot be less than 1s",
	"PeerUpdateInterval cannot be less than 10s",
	"MaxTimeForNoPeersResponse cannot be less than 1s",
}

// snrCreateCases are the SNR requests that need no stored object. The default config can exist
// already; the API server then answers AlreadyExists after admission admitted the request.
func snrCreateCases() []admissionCase {
	return append(snrConfigCreateCases(),
		admissionCase{
			desc:    "Creating a valid SelfNodeRemediationTemplate with dry-run",
			request: dryRunCreate(newSNRTemplate(snrDryRunTemplateName, "Automatic")),
			verify:  expectMultipleTemplatesAnnotation("true"),
		},
		admissionCase{
			desc: "Creating a SelfNodeRemediationTemplate that sets the multiple-templates annotation with dry-run",
			request: dryRunCreate(withAnnotation(
				newSNRTemplate(snrDryRunTemplateName, "ResourceDeletion"), multipleTemplatesAnnotation, "false")),
			verify: expectMultipleTemplatesAnnotation("false"),
		},
		admissionCase{
			desc:    "Creating a SelfNodeRemediationTemplate with the OutOfServiceTaint strategy with dry-run",
			request: dryRunCreate(newSNRTemplate(snrDryRunTemplateName, "OutOfServiceTaint")),
		},
		admissionCase{
			desc:    "Creating a SelfNodeRemediation with the OutOfServiceTaint strategy with dry-run",
			request: dryRunCreate(newSNR("OutOfServiceTaint")),
		})
}

// snrConfigCreateCases are the SelfNodeRemediationConfig create requests.
func snrConfigCreateCases() []admissionCase {
	return []admissionCase{
		{
			desc: "Creating the default SelfNodeRemediationConfig with dry-run",
			request: dryRunCreateAllowExisting(
				newSNRConfig(snrDefaultConfigName, medik8sparams.OperatorNs, nil)),
		},
		{
			desc:    "Creating a SelfNodeRemediationConfig with a different name with dry-run",
			request: dryRunCreate(newSNRConfig(waoparams.WebhookTestPrefix+"-config", medik8sparams.OperatorNs, nil)),
			webhook: snrConfigValidatingWebhook,
			denials: []string{"a name other than " + snrDefaultConfigName + " is not allowed"},
		},
		{
			desc:    "Creating a SelfNodeRemediationConfig in a different namespace with dry-run",
			request: dryRunCreate(newSNRConfig(snrDefaultConfigName, "default", nil)),
			webhook: snrConfigValidatingWebhook,
			denials: []string{"only allowed to be created in the namespace: " + medik8sparams.OperatorNs},
		},
		snrConfigDenied("Creating a SelfNodeRemediationConfig with durations below the minimum with dry-run",
			snrShortDurations(), snrShortDurationDenials...),
		snrConfigDenied("Creating a SelfNodeRemediationConfig with an invalid toleration operator with dry-run",
			snrTolerations(map[string]interface{}{"key": "test", "operator": "Invalid"}),
			"invalid operator for toleration: Invalid"),
		snrConfigDenied("Creating a SelfNodeRemediationConfig with a toleration value for Exists with dry-run",
			snrTolerations(map[string]interface{}{"key": "test", "operator": "Exists", "value": "set"}),
			"value must be empty for Operator value is Exists"),
		snrConfigDenied("Creating a SelfNodeRemediationConfig with an invalid toleration effect with dry-run",
			snrTolerations(map[string]interface{}{"key": "test", "operator": "Exists", "effect": "Invalid"}),
			"invalid taint effect for toleration: Invalid"),
		snrConfigDenied("Creating a SelfNodeRemediationConfig with a node selector In without values with dry-run",
			snrNodeSelector(map[string]interface{}{"key": "test", "operator": "In"}),
			"values must not be empty when Operator value is In"),
		snrConfigDenied("Creating a SelfNodeRemediationConfig with a node selector Gt and a text value with dry-run",
			snrNodeSelector(map[string]interface{}{"key": "test", "operator": "Gt", "values": []interface{}{"text"}}),
			"first array element must be valid integer when Operator value is Gt"),
		{
			desc: "Creating a SelfNodeRemediationConfig with an unsafe peer request timeout with dry-run",
			request: dryRunCreateAllowExisting(newSNRConfig(snrDefaultConfigName, medik8sparams.OperatorNs,
				map[string]interface{}{"apiServerTimeout": "5s", "peerRequestTimeout": "5s"})),
			warning: snrPeerTimeoutWarning,
		},
	}
}

// verifySNRRulesWithStoredDefaults checks the update and delete rules on the objects the SNR
// manager creates when it starts: the default config and a default template. Every request is a
// dry-run, so the stored objects do not change.
func verifySNRRulesWithStoredDefaults(ctx context.Context, state string) {
	GinkgoHelper()
	config := newSNRConfig(snrDefaultConfigName, medik8sparams.OperatorNs, nil)
	template := newSNRTemplate(snrDefaultTemplateName, "Automatic")

	By("Waiting for the SNR manager to create the default config and the default template")

	for _, object := range []*unstructured.Unstructured{config, template} {
		Eventually(func() error {
			return APIClient.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopy())
		}, waoparams.OperandAvailableTimeout, waoparams.DefaultPollInterval).Should(Succeed(),
			"%s %s must exist while SNR is Enabled", object.GetKind(), object.GetName())
	}

	setConfigFields := func(fields map[string]interface{}) func(object *unstructured.Unstructured) {
		return func(object *unstructured.Unstructured) {
			for name, value := range fields {
				_ = unstructured.SetNestedField(object.Object, value, "spec", name)
			}
		}
	}

	verifyAdmissionCases(ctx, state,
		admissionCase{
			desc:    "Updating the default SelfNodeRemediationConfig to durations below the minimum with dry-run",
			request: dryRunUpdate(config, setConfigFields(snrShortDurations())),
			webhook: snrConfigValidatingWebhook,
			denials: snrShortDurationDenials,
		},
		admissionCase{
			desc: "Updating the default SelfNodeRemediationConfig to an unsafe peer request timeout with dry-run",
			request: dryRunUpdate(config, setConfigFields(
				map[string]interface{}{"apiServerTimeout": "5s", "peerRequestTimeout": "5s"})),
			warning: snrPeerTimeoutWarning,
		},
		admissionCase{
			desc:    "Updating the API check interval of the default SelfNodeRemediationConfig with dry-run",
			request: dryRunUpdate(config, setConfigFields(map[string]interface{}{"apiCheckInterval": "16s"})),
		},
		admissionCase{
			desc:    "Deleting the default SelfNodeRemediationConfig with dry-run",
			request: dryRunDelete(config),
			warning: snrConfigDeletedWarning,
		},
		admissionCase{
			desc: "Updating a label of the default SelfNodeRemediationTemplate with dry-run",
			request: dryRunUpdate(template, func(object *unstructured.Unstructured) {
				object.SetLabels(map[string]string{waoparams.WebhookTestPrefix: "true"})
			}),
			verify: expectMultipleTemplatesAnnotation("true"),
		})

	By("Verifying the dry-run requests changed no stored SNR object")

	stored := config.DeepCopy()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(config), stored)).To(Succeed(),
		"SelfNodeRemediationConfig %s must still exist after the dry-run delete", snrDefaultConfigName)
	interval, _, err := unstructured.NestedString(stored.Object, "spec", "apiCheckInterval")
	Expect(err).ToNot(HaveOccurred(), "Failed to read spec.apiCheckInterval")
	Expect(interval).ToNot(Equal("16s"),
		fmt.Sprintf("SelfNodeRemediationConfig %s must keep its API check interval", snrDefaultConfigName))
}
