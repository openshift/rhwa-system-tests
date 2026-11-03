package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
