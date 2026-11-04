package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"

	appsv1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WAOConfigGVK identifies the cluster-scoped workload-availability-operator configuration.
var WAOConfigGVK = schema.GroupVersionKind{
	Group:   medik8sparams.WAOCRDGroup,
	Version: medik8sparams.WAOCRDVersion,
	Kind:    medik8sparams.WAOConfigKind,
}

// SetWAOComponents enables or disables operands of the WorkloadAvailabilityConfig singleton,
// one or several at a time, in a single merge patch. Keys of states are operand keys from
// medik8sparams (for example medik8sparams.WAOComponentNHC); the value is the desired
// spec.components.<key>.enabled.
//
// Only the named operands are touched: operands absent from states keep their current state,
// and manager settings such as resource requests are preserved for every operand. An unknown
// operand key is rejected rather than sent, because the API server silently prunes unknown
// fields and the caller would never learn the setting was dropped.
//
// Enabling an operand only starts its manager. Operands that need their own custom resource
// (NodeHealthCheck, StorageBasedRemediationConfig, NodeMaintenance) still need it created
// separately. Disabling removes the manager and the resources WAO installed, but resources
// the operand itself created can remain and are then reported in
// status.components[].retainedResources with phase DisableIncomplete.
//
// The patch records intent only. Poll GetWAOConfig and WAOComponentPhase to wait for the
// operands to reach Available or Disabled.
func SetWAOComponents(ctx context.Context, k8sClient client.Client, states map[string]bool) error {
	if k8sClient == nil {
		return fmt.Errorf("cannot configure WAO operands without a client")
	}

	if len(states) == 0 {
		return fmt.Errorf("no WAO operands given; valid operands: %s", strings.Join(medik8sparams.WAOAllComponents, ", "))
	}

	components := make(map[string]interface{}, len(states))

	for component, enabled := range states {
		if !slices.Contains(medik8sparams.WAOAllComponents, component) {
			return fmt.Errorf("unknown WAO operand %q; valid operands: %s",
				component, strings.Join(medik8sparams.WAOAllComponents, ", "))
		}

		components[component] = map[string]interface{}{"enabled": enabled}
	}

	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"components": components},
	})
	if err != nil {
		return fmt.Errorf("build WAO operand patch: %w", err)
	}

	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(WAOConfigGVK)
	config.SetName(medik8sparams.WAOConfigName)

	if patchErr := k8sClient.Patch(ctx, config, client.RawPatch(types.MergePatchType, patch)); patchErr != nil {
		if k8serrors.IsNotFound(patchErr) {
			return fmt.Errorf(
				"%s %q not found: WAO creates this singleton on startup, so the operator is not installed: %w",
				medik8sparams.WAOConfigKind, medik8sparams.WAOConfigName, patchErr)
		}

		return fmt.Errorf("configure WAO operands %s: %w", strings.Join(sortedKeys(states), ", "), patchErr)
	}

	return nil
}

// EnableWAOComponents enables the named operands in one patch, leaving every other operand
// untouched. Passing no operand is an error; use medik8sparams.WAOAllComponents to enable all.
func EnableWAOComponents(ctx context.Context, k8sClient client.Client, components ...string) error {
	return SetWAOComponents(ctx, k8sClient, waoComponentStates(true, components))
}

// DisableWAOComponents disables the named operands in one patch, leaving every other operand
// untouched. Disable an operand only when it has no active maintenance or remediation.
func DisableWAOComponents(ctx context.Context, k8sClient client.Client, components ...string) error {
	return SetWAOComponents(ctx, k8sClient, waoComponentStates(false, components))
}

// ErrWAONotInstalled reports a cluster with no WorkloadAvailabilityConfig singleton: either the
// CRD is absent or WAO has not bootstrapped the CR. A run that asked for the WAO layout with
// labels.OperatorWAO and lands on such a cluster has nothing to enable, so this is a failure
// rather than something to work around.
var ErrWAONotInstalled = errors.New("workload-availability-operator is not installed")

// WAORunRequested reports whether this run asked for the workload-availability-operator layout,
// which is what makes a suite enable its operand before its specs. Operand enablement is opt-in:
// on a pre-WAO cluster the operators are installed individually and there is nothing to enable,
// and a suite that patched a WorkloadAvailabilityConfig there would fail for the wrong reason.
//
// The opt-in is labels.OperatorWAO appearing in the run's Ginkgo label filter — ECO_TEST_LABELS
// when the run came through scripts/test-runner.sh, otherwise ginkgoLabelFilter, the
// --label-filter given to ginkgo directly. Because a suite's own specs carry their operator
// label and not labels.OperatorWAO, select both:
//
//	ECO_TEST_LABELS='operator:wao || operator:snr'
//
// A filter that negates the label (!operator:wao) does not request the WAO layout.
func WAORunRequested(ginkgoLabelFilter string) bool {
	filter := os.Getenv("ECO_TEST_LABELS")
	if filter == "" {
		filter = ginkgoLabelFilter
	}

	isSeparator := func(r rune) bool {
		return r == '&' || r == '|' || r == '(' || r == ')' || r == ',' || unicode.IsSpace(r)
	}
	for _, term := range strings.FieldsFunc(filter, isSeparator) {
		if term == labels.OperatorWAO {
			return true
		}
	}

	return false
}

// EnsureWAOComponents makes every named operand ready to be tested: enabled in the
// WorkloadAvailabilityConfig and reporting phase Available. It is the precondition a suite runs
// before its first spec, and it is idempotent — an operand that is already enabled and Available
// is not patched, so a suite run back to back does not restart a manager it is about to use.
//
// Operands that need a patch are enabled in one call, then every named operand is waited on.
// An operand left in DisableIncomplete by an earlier run is enabled again like any other.
//
// Enabling starts the manager only. Operand custom resources (NodeHealthCheck,
// StorageBasedRemediationConfig, NodeMaintenance) are still the spec's to create.
//
// On a cluster without the singleton it returns an error matching ErrWAONotInstalled.
func EnsureWAOComponents(ctx context.Context, k8sClient client.Client, components ...string) error {
	if len(components) == 0 {
		return fmt.Errorf("no WAO operands given; valid operands: %s", strings.Join(medik8sparams.WAOAllComponents, ", "))
	}

	config, err := GetWAOConfig(ctx, k8sClient)
	if err != nil {
		if k8serrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return fmt.Errorf("%w: %w", ErrWAONotInstalled, err)
		}

		return err
	}

	var pending []string

	for _, component := range components {
		enabled, enabledErr := WAOComponentEnabled(config, component)
		if enabledErr != nil {
			return enabledErr
		}

		phase, phaseErr := WAOComponentPhase(config, component)
		if phaseErr != nil {
			return phaseErr
		}

		if !enabled || phase != medik8sparams.WAOPhaseAvailable {
			pending = append(pending, component)
		}
	}

	if len(pending) > 0 {
		if enableErr := EnableWAOComponents(ctx, k8sClient, pending...); enableErr != nil {
			return enableErr
		}
	}

	return WaitForWAOComponentsAvailable(ctx, k8sClient, components...)
}

// WaitForWAOComponentsAvailable blocks until every named operand reports phase Available, or
// until medik8sparams.WAOOperandAvailableTimeout elapses. A timeout names the operand and the
// phase it was last seen in, which is what tells a Degraded install apart from a slow one.
func WaitForWAOComponentsAvailable(ctx context.Context, k8sClient client.Client, components ...string) error {
	for _, component := range components {
		lastPhase := "<none reported>"
		condition := func(ctx context.Context) (bool, error) {
			config, err := GetWAOConfig(ctx, k8sClient)
			if err != nil {
				return false, err
			}

			phase, phaseErr := WAOComponentPhase(config, component)
			if phaseErr != nil {
				return false, phaseErr
			}

			if phase != "" {
				lastPhase = phase
			}

			return phase == medik8sparams.WAOPhaseAvailable, nil
		}

		err := wait.PollUntilContextTimeout(ctx, medik8sparams.WAOPollInterval,
			medik8sparams.WAOOperandAvailableTimeout, true, condition)
		if err != nil {
			return fmt.Errorf("WAO operand %q did not reach phase %s within %s, last phase %q: %w",
				component, medik8sparams.WAOPhaseAvailable, medik8sparams.WAOOperandAvailableTimeout,
				lastPhase, err)
		}
	}

	return nil
}

// WAOComponentRunning reports whether an operand is installed and running, by checking that
// every Deployment in medik8sparams.WAOOperandDeployments exists and has at least one ready
// replica. The returned string describes what was found, ready for a log line:
// "self-node-remediation-controller-manager 2/2" or "... not found".
//
// Use this, not the presence of the operand's CRD, to decide whether another operator is on the
// cluster. Under WAO, OLM installs every operand CRD with the bundle and leaves it there across
// enable and disable, so a CRD check reports every operand as installed on every WAO cluster.
// A Deployment check gives the same answer on both layouts, because the Deployment name belongs
// to the operand rather than to whatever installed it.
func WAOComponentRunning(ctx context.Context, k8sClient client.Client, component string) (bool, string, error) {
	if k8sClient == nil {
		return false, "", fmt.Errorf("cannot check operand %q without a client", component)
	}

	names, known := medik8sparams.WAOOperandDeployments[component]
	if !known {
		return false, "", fmt.Errorf("unknown WAO operand %q; valid operands: %s",
			component, strings.Join(medik8sparams.WAOAllComponents, ", "))
	}

	running := true
	found := make([]string, 0, len(names))

	for _, name := range names {
		operandDeployment := &appsv1.Deployment{}
		err := k8sClient.Get(ctx,
			client.ObjectKey{Namespace: medik8sparams.OperatorNs, Name: name}, operandDeployment)
		if k8serrors.IsNotFound(err) {
			running = false
			found = append(found, name+" not found")

			continue
		}

		if err != nil {
			return false, "", fmt.Errorf("read Deployment %s/%s: %w", medik8sparams.OperatorNs, name, err)
		}

		if operandDeployment.Status.ReadyReplicas < 1 {
			running = false
		}

		found = append(found, fmt.Sprintf("%s %d/%d",
			name, operandDeployment.Status.ReadyReplicas, operandDeployment.Status.Replicas))
	}

	return running, strings.Join(found, ", "), nil
}

// GetWAOConfig reads the WorkloadAvailabilityConfig singleton.
func GetWAOConfig(ctx context.Context, k8sClient client.Client) (*unstructured.Unstructured, error) {
	if k8sClient == nil {
		return nil, fmt.Errorf("cannot read the WAO configuration without a client")
	}

	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(WAOConfigGVK)

	if err := k8sClient.Get(ctx, client.ObjectKey{Name: medik8sparams.WAOConfigName}, config); err != nil {
		return nil, fmt.Errorf("read %s %q: %w", medik8sparams.WAOConfigKind, medik8sparams.WAOConfigName, err)
	}

	return config, nil
}

// WAOComponentEnabled reports the configured spec intent for one operand. Operands that are
// unset default to disabled.
func WAOComponentEnabled(config *unstructured.Unstructured, component string) (bool, error) {
	if config == nil {
		return false, fmt.Errorf("no WAO configuration to read operand %q from", component)
	}

	enabled, _, err := unstructured.NestedBool(config.Object, "spec", "components", component, "enabled")
	if err != nil {
		return false, fmt.Errorf("read WAO operand %q intent: %w", component, err)
	}

	return enabled, nil
}

// WAOComponentPhase returns the observed phase of one operand from status.components, for
// example medik8sparams.WAOPhaseAvailable. An operand with no status entry yet returns an
// empty phase and no error, so callers can poll until WAO reports it.
func WAOComponentPhase(config *unstructured.Unstructured, component string) (string, error) {
	if config == nil {
		return "", fmt.Errorf("no WAO configuration to read operand %q status from", component)
	}

	statuses, _, err := unstructured.NestedSlice(config.Object, "status", "components")
	if err != nil {
		return "", fmt.Errorf("read WAO operand statuses: %w", err)
	}

	for _, status := range statuses {
		statusMap, ok := status.(map[string]interface{})
		if !ok {
			continue
		}

		if name, _, _ := unstructured.NestedString(statusMap, "name"); name != component {
			continue
		}

		phase, _, phaseErr := unstructured.NestedString(statusMap, "phase")
		if phaseErr != nil {
			return "", fmt.Errorf("read WAO operand %q phase: %w", component, phaseErr)
		}

		return phase, nil
	}

	return "", nil
}

// WAOComponentRetainedResources lists what outlived a disable of one operand: agent DaemonSets,
// their native configs, and native objects the stopped manager would have reconciled. A non-empty
// list is why an operand reports DisableIncomplete instead of Disabled.
func WAOComponentRetainedResources(config *unstructured.Unstructured, component string) ([]string, error) {
	if config == nil {
		return nil, fmt.Errorf("no WAO configuration to read operand %q status from", component)
	}

	statuses, _, err := unstructured.NestedSlice(config.Object, "status", "components")
	if err != nil {
		return nil, fmt.Errorf("read WAO operand statuses: %w", err)
	}

	for _, status := range statuses {
		statusMap, ok := status.(map[string]interface{})
		if !ok {
			continue
		}

		if name, _, _ := unstructured.NestedString(statusMap, "name"); name != component {
			continue
		}

		retained, _, retainedErr := unstructured.NestedStringSlice(statusMap, "retainedResources")
		if retainedErr != nil {
			return nil, fmt.Errorf("read WAO operand %q retained resources: %w", component, retainedErr)
		}

		return retained, nil
	}

	return nil, nil
}

// waoComponentStates maps every named operand to the same desired state.
func waoComponentStates(enabled bool, components []string) map[string]bool {
	states := make(map[string]bool, len(components))
	for _, component := range components {
		states[component] = enabled
	}

	return states
}

// sortedKeys gives error messages a stable operand order.
func sortedKeys(states map[string]bool) []string {
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}
