package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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

// ECOTestLabelsEnvVar is the environment variable that carries the Ginkgo label-filter expression
// (ginkgo --label-filter) for a run, for example "operator:far && tier:smoke". The WAO precondition
// reads it to learn which operands the selected specs need.
const ECOTestLabelsEnvVar = "ECO_TEST_LABELS"

// operatorLabelToComponent maps each operator:<x> label to the WorkloadAvailabilityConfig operand
// key it enables. operator:wao (WAO itself, always installed) and operator:interop (cross-operator,
// no single operand) have no entry, so they enable nothing on their own.
var operatorLabelToComponent = map[string]string{
	labels.OperatorNHC: medik8sparams.WAOComponentNHC,
	labels.OperatorSNR: medik8sparams.WAOComponentSNR,
	labels.OperatorSBR: medik8sparams.WAOComponentSBR,
	labels.OperatorNMO: medik8sparams.WAOComponentNMO,
	labels.OperatorFAR: medik8sparams.WAOComponentFAR,
	labels.OperatorMDR: medik8sparams.WAOComponentMDR,
}

// operatorLabelExpr matches an operator:<slug> label in a Ginkgo label-filter expression, capturing
// a leading "!" so a negated term (!operator:far) can be told apart and skipped.
var operatorLabelExpr = regexp.MustCompile(`(!\s*)?\b(operator:[a-z]+)\b`)

// WAOOperandsForLabelFilter returns the WorkloadAvailabilityConfig operand keys named by positive
// operator:<x> terms in a Ginkgo label-filter expression (the ECO_TEST_LABELS value). Negated terms
// (!operator:x) and operator labels with no single operand (operator:wao, operator:interop) are
// ignored. The result is sorted and deduplicated; a filter that names no mappable operator returns
// nil, so a run selected only by tier or frequency enables nothing.
func WAOOperandsForLabelFilter(labelFilter string) []string {
	seen := make(map[string]bool)

	for _, match := range operatorLabelExpr.FindAllStringSubmatch(labelFilter, -1) {
		if match[1] != "" {
			// Negated term such as !operator:far: the run excludes this operator.
			continue
		}

		component, ok := operatorLabelToComponent[match[2]]
		if !ok {
			// operator:wao and operator:interop name no single operand.
			continue
		}

		seen[component] = true
	}

	if len(seen) == 0 {
		return nil
	}

	operands := make([]string, 0, len(seen))
	for component := range seen {
		operands = append(operands, component)
	}
	slices.Sort(operands)

	return operands
}

// EnsureWAOOperandsEnabled is a suite precondition: it makes the operands named by the
// ECO_TEST_LABELS label filter enabled and Available before the first spec runs, and returns the
// operands it waited on. It implements the precondition contract in docs/TESTCASES/AGENTS.md:
//
//   - A filter that names no mappable operand enables nothing and returns nil.
//   - On a pre-WAO cluster (the WorkloadAvailabilityConfig singleton is absent) it is a no-op, so a
//     spec runs unchanged on either layout and falls back to its own operator check.
//   - It is idempotent: an operand already enabled and Available is left untouched; only operands
//     that are not yet Available are enabled, in a single patch.
//   - It blocks until every requested operand reports phase Available, bounded by availableTimeout
//     and pollInterval, and fails fast if one reports Degraded.
//
// Pass waoparams.OperandAvailableTimeout and waoparams.DefaultPollInterval for the bounds. The
// caller keeps its own deployment.Pull/IsReady readiness check: Available is the operand's own
// report, and the readiness check remains the externally observable assertion.
func EnsureWAOOperandsEnabled(ctx context.Context, k8sClient client.Client,
	availableTimeout, pollInterval time.Duration,
) ([]string, error) {
	operands := WAOOperandsForLabelFilter(os.Getenv(ECOTestLabelsEnvVar))
	if len(operands) == 0 {
		return nil, nil
	}

	config, err := GetWAOConfig(ctx, k8sClient)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Pre-WAO cluster: the singleton does not exist, so the operators are installed
			// individually and there is no operand to enable.
			return nil, nil
		}

		return nil, err
	}

	var toEnable []string

	for _, operand := range operands {
		enabled, enabledErr := WAOComponentEnabled(config, operand)
		if enabledErr != nil {
			return nil, enabledErr
		}

		phase, phaseErr := WAOComponentPhase(config, operand)
		if phaseErr != nil {
			return nil, phaseErr
		}

		if !enabled || phase != medik8sparams.WAOPhaseAvailable {
			toEnable = append(toEnable, operand)
		}
	}

	if len(toEnable) > 0 {
		if enableErr := EnableWAOComponents(ctx, k8sClient, toEnable...); enableErr != nil {
			return nil, enableErr
		}
	}

	if waitErr := waitForWAOOperandsAvailable(
		ctx, k8sClient, operands, availableTimeout, pollInterval); waitErr != nil {
		return nil, waitErr
	}

	return operands, nil
}

// waitForWAOOperandsAvailable blocks until every named operand reports phase Available. It returns
// an error if any operand reports Degraded or if the timeout elapses first.
func waitForWAOOperandsAvailable(ctx context.Context, k8sClient client.Client, operands []string,
	timeout, pollInterval time.Duration,
) error {
	var pending string

	waitErr := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			config, err := GetWAOConfig(ctx, k8sClient)
			if err != nil {
				return false, err
			}

			pending = ""

			for _, operand := range operands {
				phase, phaseErr := WAOComponentPhase(config, operand)
				if phaseErr != nil {
					return false, phaseErr
				}

				if phase == medik8sparams.WAOPhaseDegraded {
					return false, fmt.Errorf("operand %q reported phase %s", operand, medik8sparams.WAOPhaseDegraded)
				}

				if phase != medik8sparams.WAOPhaseAvailable {
					pending = operand

					return false, nil
				}
			}

			return true, nil
		})
	if waitErr != nil {
		if pending != "" {
			return fmt.Errorf("operand %q did not reach phase %s within %s: %w",
				pending, medik8sparams.WAOPhaseAvailable, timeout, waitErr)
		}

		return fmt.Errorf("waiting for WAO operands to reach phase %s: %w",
			medik8sparams.WAOPhaseAvailable, waitErr)
	}

	return nil
}
