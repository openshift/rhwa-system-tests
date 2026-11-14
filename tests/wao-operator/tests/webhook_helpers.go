package tests

import (
	"context"
	"fmt"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/wao-operator/internal/waoparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	farAPIGroup = "fence-agents-remediation.medik8s.io"
	nhcAPIGroup = "remediation.medik8s.io"
	nmoAPIGroup = "nodemaintenance.medik8s.io"
	snrAPIGroup = "self-node-remediation.medik8s.io"
	mdrAPIGroup = "machine-deletion-remediation.medik8s.io"

	// multipleTemplatesAnnotation is set by the FAR and SNR template defaulters and read by NHC
	// admission for the duplicate template kind rule.
	multipleTemplatesAnnotation = "remediation.medik8s.io/multiple-templates-support"

	// snrDefaultConfigName is the only name SNR admission accepts for a SelfNodeRemediationConfig.
	snrDefaultConfigName = "self-node-remediation-config"

	// snrDefaultTemplateName is a template the SNR manager creates when it starts.
	snrDefaultTemplateName = "self-node-remediation-automatic-strategy-template"

	workerRoleLabel       = "node-role.kubernetes.io/worker"
	controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"

	stateDisabled = "Disabled"
	stateEnabled  = "Enabled"
)

var (
	farGVK         = schema.GroupVersionKind{Group: farAPIGroup, Version: "v1alpha1", Kind: "FenceAgentsRemediation"}
	farTemplateGVK = schema.GroupVersionKind{
		Group: farAPIGroup, Version: "v1alpha1", Kind: "FenceAgentsRemediationTemplate",
	}
	nhcGVK         = schema.GroupVersionKind{Group: nhcAPIGroup, Version: "v1alpha1", Kind: "NodeHealthCheck"}
	nmGVK          = schema.GroupVersionKind{Group: nmoAPIGroup, Version: "v1beta1", Kind: "NodeMaintenance"}
	snrGVK         = schema.GroupVersionKind{Group: snrAPIGroup, Version: "v1alpha1", Kind: "SelfNodeRemediation"}
	snrConfigGVK   = schema.GroupVersionKind{Group: snrAPIGroup, Version: "v1alpha1", Kind: "SelfNodeRemediationConfig"}
	snrTemplateGVK = schema.GroupVersionKind{
		Group: snrAPIGroup, Version: "v1alpha1", Kind: "SelfNodeRemediationTemplate",
	}
	mdrTemplateGVK = schema.GroupVersionKind{
		Group: mdrAPIGroup, Version: "v1alpha1", Kind: "MachineDeletionRemediationTemplate",
	}
)

// warningRecorder collects the warnings the API server returns, which is how an admission
// webhook reports a concern about a request it admits.
type warningRecorder struct {
	lock     sync.Mutex
	warnings []string
}

// HandleWarningHeader implements rest.WarningHandler.
func (recorder *warningRecorder) HandleWarningHeader(_ int, _ string, text string) {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	recorder.warnings = append(recorder.warnings, text)
}

// take returns the warnings recorded since the previous call.
func (recorder *warningRecorder) take() []string {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	taken := recorder.warnings
	recorder.warnings = nil

	return taken
}

var (
	webhookClientOnce sync.Once
	errWebhookClient  error
	webhookK8sClient  client.Client
	webhookWarnings   = &warningRecorder{}
)

// webhookClient returns the client the admission requests use. It differs from APIClient only
// in that it records API server warnings instead of logging them.
func webhookClient() client.Client {
	GinkgoHelper()
	webhookClientOnce.Do(func() {
		config := rest.CopyConfig(APIClient.Config)
		config.WarningHandler = webhookWarnings
		webhookK8sClient, errWebhookClient = client.New(config, client.Options{})
	})
	Expect(errWebhookClient).ToNot(HaveOccurred(), "Failed to build the client for admission requests")

	return webhookK8sClient
}

// admissionRequest sends one request and returns the object the API server answered with.
type admissionRequest func(ctx context.Context, k8sClient client.Client) (*unstructured.Unstructured, error)

// admissionCase is one request and the answer admission must give. A case with no denial must
// be admitted.
type admissionCase struct {
	desc    string
	request admissionRequest
	// webhook is the entry that must answer a denied or failed request.
	webhook string
	// denials are substrings that the denial must contain, all of them.
	denials []string
	// callFails means the API server must report that the webhook call itself failed, which is
	// the answer of an entry with failurePolicy Fail when the listener does not answer.
	callFails bool
	// warning is a substring of a warning the API server must return.
	warning string
	// verify checks the admitted object, for example a field a mutating webhook sets.
	verify func(admitted *unstructured.Unstructured) error
}

// evaluate returns why the answer is wrong, or an empty string.
func (testCase admissionCase) evaluate(admitted *unstructured.Unstructured, err error, warnings []string) string {
	if testCase.callFails {
		return testCase.evaluateFailedCall(err)
	}

	if len(testCase.denials) == 0 && err != nil {
		return fmt.Sprintf("the request must be admitted, got: %v", err)
	}

	if len(testCase.denials) > 0 {
		if err == nil {
			return fmt.Sprintf("the request was admitted, want a denial containing %q", testCase.denials)
		}

		for _, want := range append([]string{fmt.Sprintf("%q", testCase.webhook)}, testCase.denials...) {
			if !strings.Contains(err.Error(), want) {
				return fmt.Sprintf("the answer does not contain %s: %v", want, err)
			}
		}
	}

	if testCase.warning != "" && !strings.Contains(strings.Join(warnings, "\n"), testCase.warning) {
		return fmt.Sprintf("no warning contains %q, got: %q", testCase.warning, warnings)
	}

	if err == nil && testCase.verify != nil {
		if verifyErr := testCase.verify(admitted); verifyErr != nil {
			return verifyErr.Error()
		}
	}

	return ""
}

// evaluateFailedCall returns why the answer is not a failed webhook call, or an empty string. The
// API server answers a failed call with an internal error. It names the webhook, except when the
// timeout of the entry is longer than the time the API server gives to the whole validating
// phase: then the phase ends first and the answer names the admission plugin instead.
func (testCase admissionCase) evaluateFailedCall(err error) string {
	if err == nil {
		return "the request was admitted, want a failed webhook call"
	}

	if !k8serrors.IsInternalError(err) {
		return fmt.Sprintf("the answer is not an internal error of a failed webhook call: %v", err)
	}

	if !strings.Contains(err.Error(), fmt.Sprintf("failed calling webhook %q", testCase.webhook)) &&
		!strings.Contains(err.Error(), "failed to complete validation") {
		return fmt.Sprintf("the answer does not name a failed call of %s: %v", testCase.webhook, err)
	}

	return ""
}

// admissionFailures sends every case and returns one line per wrong answer. state names the
// condition the requests are sent in and becomes part of each step.
func admissionFailures(ctx context.Context, state string, cases []admissionCase) []string {
	GinkgoHelper()
	k8sClient := webhookClient()

	var failures []string

	for _, testCase := range cases {
		By(fmt.Sprintf("%s [%s]", testCase.desc, state))
		webhookWarnings.take()
		admitted, err := testCase.request(ctx, k8sClient)

		if failure := testCase.evaluate(admitted, err, webhookWarnings.take()); failure != "" {
			failures = append(failures, fmt.Sprintf("%s [%s]: %s", testCase.desc, state, failure))
		}
	}

	return failures
}

// verifyAdmissionCases sends every case and fails once at the end, so one wrong answer does not
// hide the answers to the remaining requests.
func verifyAdmissionCases(ctx context.Context, state string, cases ...admissionCase) {
	GinkgoHelper()

	if failures := admissionFailures(ctx, state, cases); len(failures) > 0 {
		Fail("admission gave wrong answers:\n- " + strings.Join(failures, "\n- "))
	}
}

// dryRunCreate sends a server-side dry-run create: the API server calls admission and stores
// nothing.
func dryRunCreate(object *unstructured.Unstructured) admissionRequest {
	return func(ctx context.Context, k8sClient client.Client) (*unstructured.Unstructured, error) {
		request := object.DeepCopy()

		return request, k8sClient.Create(ctx, request, client.DryRunAll)
	}
}

// dryRunCreateAllowExisting is dryRunCreate for an object that can exist already. The API server
// reports AlreadyExists only after admission admitted the request, so that answer counts as
// admitted.
func dryRunCreateAllowExisting(object *unstructured.Unstructured) admissionRequest {
	return func(ctx context.Context, k8sClient client.Client) (*unstructured.Unstructured, error) {
		request, err := dryRunCreate(object)(ctx, k8sClient)
		if k8serrors.IsAlreadyExists(err) {
			return request, nil
		}

		return request, err
	}
}

// dryRunUpdate reads a stored object, changes it and sends a server-side dry-run update.
func dryRunUpdate(stored *unstructured.Unstructured, change func(object *unstructured.Unstructured)) admissionRequest {
	return func(ctx context.Context, k8sClient client.Client) (*unstructured.Unstructured, error) {
		request := &unstructured.Unstructured{}
		request.SetGroupVersionKind(stored.GroupVersionKind())

		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stored), request); err != nil {
			return nil, fmt.Errorf("read %s %q before the update: %w", stored.GetKind(), stored.GetName(), err)
		}

		change(request)

		return request, k8sClient.Update(ctx, request, client.DryRunAll)
	}
}

// dryRunDelete sends a server-side dry-run delete of a stored object.
func dryRunDelete(stored *unstructured.Unstructured) admissionRequest {
	return func(ctx context.Context, k8sClient client.Client) (*unstructured.Unstructured, error) {
		request := stored.DeepCopy()

		return request, k8sClient.Delete(ctx, request, client.DryRunAll)
	}
}

// removeObject deletes an object and waits until it is gone. A missing object is not an error.
func removeObject(ctx context.Context, object client.Object) error {
	if err := APIClient.Delete(ctx, object); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", object.GetName(), err)
	}

	current, isObject := object.DeepCopyObject().(client.Object)
	if !isObject {
		return fmt.Errorf("%s is not a client object", object.GetName())
	}

	return wait.PollUntilContextTimeout(ctx, waoparams.DefaultPollInterval, waoparams.StoredObjectTimeout, true,
		func(ctx context.Context) (bool, error) {
			err := APIClient.Get(ctx, client.ObjectKeyFromObject(object), current)
			if k8serrors.IsNotFound(err) {
				return true, nil
			}

			return false, err
		})
}

// removeObjectsInCleanup removes stored objects after a spec, also a failed one. It reports a
// problem instead of asserting on it, so cleanup cannot hide the failure of the spec.
func removeObjectsInCleanup(objects ...client.Object) {
	for _, object := range objects {
		if err := removeObject(context.Background(), object); err != nil {
			GinkgoWriter.Printf("DeferCleanup: %v\n", err)
		}
	}
}

// operandIsEnabled reads the configured intent of one operand.
func operandIsEnabled(operand string) bool {
	GinkgoHelper()
	config, err := helpers.GetWAOConfig(context.Background(), APIClient)
	Expect(err).ToNot(HaveOccurred(), "WAO is not installed on this cluster")
	enabled, err := helpers.WAOComponentEnabled(config, operand)
	Expect(err).ToNot(HaveOccurred(), "Failed to read the intent of operand %s", operand)

	return enabled
}

// expectAdmissionReady waits for WAO to report that its listener serves the admission of one
// operand.
func expectAdmissionReady(operand string) {
	GinkgoHelper()

	By(fmt.Sprintf("Verifying WAO reports admissionReady for operand %s", operand))
	Eventually(func() (bool, error) {
		config, err := helpers.GetWAOConfig(context.Background(), APIClient)
		if err != nil {
			return false, err
		}

		return helpers.WAOComponentAdmissionReady(config, operand)
	}, waoparams.OperandAvailableTimeout, waoparams.DefaultPollInterval).Should(BeTrue(),
		"status.components[%s].admissionReady must be true", operand)
}

// restoreOperandIntent returns one operand to the intent the spec found. It runs after a failure
// too, so it reports problems instead of asserting on them. safe guards the enable: when it
// returns false the operand stays disabled, because an object of the spec is still stored and an
// enabled manager would act on it.
func restoreOperandIntent(operand string, enabled bool, safe func() bool) {
	if enabled && safe != nil && !safe() {
		GinkgoWriter.Printf("DeferCleanup: operand %s stays disabled: an object of the spec is still stored; "+
			"delete it, then enable the operand again\n", operand)
		enabled = false
	}

	if err := helpers.SetWAOComponents(
		context.Background(), APIClient, map[string]bool{operand: enabled}); err != nil {
		GinkgoWriter.Printf("DeferCleanup: failed to restore operand %s to enabled=%v: %v\n", operand, enabled, err)
	}
}

// operandAdmissionPhases runs the requests of one operand spec in both states of the operand. It
// disables the operand first when the spec found it enabled, and ends with the operand in the
// state it found. whileDisabled may store objects; they must be gone when it returns, and safe
// tells the cleanup whether that is true.
func operandAdmissionPhases(operand string, whileDisabled, whileEnabled func(state string), safe func() bool) {
	GinkgoHelper()

	By(fmt.Sprintf("Recording the state of operand %s", operand))

	wasEnabled := operandIsEnabled(operand)

	DeferCleanup(func() {
		By(fmt.Sprintf("DeferCleanup: restoring operand %s", operand))
		restoreOperandIntent(operand, wasEnabled, safe)
	})

	if wasEnabled {
		disableOperand(operand)
	}

	expectAdmissionReady(operand)
	whileDisabled(operand + " " + stateDisabled)
	enableOperand(operand)
	expectAdmissionReady(operand)
	whileEnabled(operand + " " + stateEnabled)

	if !wasEnabled {
		disableOperand(operand)
	}
}

// newObject builds an unstructured object. An empty namespace gives a cluster-scoped object.
func newObject(
	gvk schema.GroupVersionKind, name, namespace string, spec map[string]interface{},
) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	object.SetGroupVersionKind(gvk)
	object.SetName(name)
	object.SetNamespace(namespace)

	return object
}

// withAnnotation returns the object with one annotation set.
func withAnnotation(object *unstructured.Unstructured, key, value string) *unstructured.Unstructured {
	object.SetAnnotations(map[string]string{key: value})

	return object
}

// expectMultipleTemplatesAnnotation returns a verify function for the value of the annotation
// the template defaulters set.
func expectMultipleTemplatesAnnotation(want string) func(admitted *unstructured.Unstructured) error {
	return func(admitted *unstructured.Unstructured) error {
		if got := admitted.GetAnnotations()[multipleTemplatesAnnotation]; got != want {
			return fmt.Errorf("annotation %s is %q, want %q", multipleTemplatesAnnotation, got, want)
		}

		return nil
	}
}

// templateRef builds a remediation template reference for a NodeHealthCheck.
func templateRef(gvk schema.GroupVersionKind, name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": gvk.GroupVersion().String(),
		"kind":       gvk.Kind,
		"name":       name,
		"namespace":  medik8sparams.OperatorNs,
	}
}

// firstNodeName returns the name of one node that has the role label and, for a worker, is not
// also a control plane node.
func firstNodeName(ctx context.Context, roleLabel string) string {
	GinkgoHelper()
	nodes := &corev1.NodeList{}
	Expect(APIClient.List(ctx, nodes, client.HasLabels{roleLabel})).To(Succeed(),
		"Failed to list nodes with label %s", roleLabel)

	for idx := range nodes.Items {
		_, isControlPlane := nodes.Items[idx].Labels[controlPlaneRoleLabel]
		if roleLabel == controlPlaneRoleLabel || !isControlPlane {
			return nodes.Items[idx].Name
		}
	}

	Fail(fmt.Sprintf("No node with label %s found", roleLabel))

	return ""
}

// probeCases is one valid request for each kind WAO serves admission for. Every request is a
// dry-run and names no real node except the NodeMaintenance, which a dry-run does not store.
func probeCases(ctx context.Context) []admissionCase {
	GinkgoHelper()

	return []admissionCase{
		{desc: "Creating a valid FenceAgentsRemediation with dry-run", request: dryRunCreate(newFAR(farSpec()))},
		{
			desc:    "Creating a valid FenceAgentsRemediationTemplate with dry-run",
			request: dryRunCreate(newFARTemplate(waoparams.WebhookTestPrefix+"-probe", farSpec())),
		},
		{desc: "Creating a valid NodeHealthCheck with dry-run", request: dryRunCreate(newNHC(nil))},
		{
			desc:    "Creating a NodeMaintenance for a worker node with dry-run",
			request: dryRunCreate(newNodeMaintenance(waoparams.WebhookTestPrefix+"-probe", firstNodeName(ctx, workerRoleLabel))),
		},
		{
			desc: "Creating the default SelfNodeRemediationConfig with dry-run",
			request: dryRunCreateAllowExisting(
				newObject(snrConfigGVK, snrDefaultConfigName, medik8sparams.OperatorNs, map[string]interface{}{})),
		},
		{
			desc:    "Creating a valid SelfNodeRemediationTemplate with dry-run",
			request: dryRunCreate(newSNRTemplate(waoparams.WebhookTestPrefix+"-probe", "Automatic")),
		},
		{desc: "Creating a SelfNodeRemediation with dry-run", request: dryRunCreate(newSNR("Automatic"))},
	}
}
