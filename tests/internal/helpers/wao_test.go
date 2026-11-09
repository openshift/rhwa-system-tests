package helpers

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// waoConfig builds the singleton with the given spec.components content.
func waoConfig(components map[string]interface{}) *unstructured.Unstructured {
	config := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"components": components},
	}}
	config.SetGroupVersionKind(WAOConfigGVK)
	config.SetName(medik8sparams.WAOConfigName)

	return config
}

func waoClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(WAOConfigGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(WAOConfigGVK.GroupVersion().WithKind(WAOConfigGVK.Kind+"List"),
		&unstructured.UnstructuredList{})

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func waoEnabled(t *testing.T, k8sClient client.Client, component string) bool {
	t.Helper()
	config, err := GetWAOConfig(context.Background(), k8sClient)
	if err != nil {
		t.Fatalf("GetWAOConfig: %v", err)
	}

	enabled, err := WAOComponentEnabled(config, component)
	if err != nil {
		t.Fatalf("WAOComponentEnabled(%s): %v", component, err)
	}

	return enabled
}

func TestSetWAOComponentsEnablesAndDisablesTogether(t *testing.T) {
	k8sClient := waoClient(waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentSNR: map[string]interface{}{"enabled": true},
	}))

	if err := SetWAOComponents(context.Background(), k8sClient, map[string]bool{
		medik8sparams.WAOComponentNHC: true,
		medik8sparams.WAOComponentFAR: true,
		medik8sparams.WAOComponentSNR: false,
	}); err != nil {
		t.Fatalf("SetWAOComponents: %v", err)
	}

	for component, want := range map[string]bool{
		medik8sparams.WAOComponentNHC: true,
		medik8sparams.WAOComponentFAR: true,
		medik8sparams.WAOComponentSNR: false,
	} {
		if got := waoEnabled(t, k8sClient, component); got != want {
			t.Errorf("operand %s enabled = %v, want %v", component, got, want)
		}
	}
}

func TestSetWAOComponentsPreservesUntouchedSettings(t *testing.T) {
	k8sClient := waoClient(waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentMDR: map[string]interface{}{"enabled": true},
		medik8sparams.WAOComponentNHC: map[string]interface{}{
			"enabled": false,
			"manager": map[string]interface{}{
				"resources": map[string]interface{}{"requests": map[string]interface{}{"memory": "64Mi"}},
			},
		},
	}))

	if err := EnableWAOComponents(context.Background(), k8sClient, medik8sparams.WAOComponentNHC); err != nil {
		t.Fatalf("EnableWAOComponents: %v", err)
	}

	config, err := GetWAOConfig(context.Background(), k8sClient)
	if err != nil {
		t.Fatalf("GetWAOConfig: %v", err)
	}

	memory, found, err := unstructured.NestedString(config.Object,
		"spec", "components", medik8sparams.WAOComponentNHC, "manager", "resources", "requests", "memory")
	if err != nil || !found || memory != "64Mi" {
		t.Errorf("manager resources = %q (found %v, err %v), want 64Mi preserved", memory, found, err)
	}

	if !waoEnabled(t, k8sClient, medik8sparams.WAOComponentNHC) {
		t.Error("nodeHealthCheck was not enabled")
	}

	if !waoEnabled(t, k8sClient, medik8sparams.WAOComponentMDR) {
		t.Error("machineDeletionRemediation lost its enabled state")
	}
}

func TestDisableWAOComponentsAcceptsEveryOperand(t *testing.T) {
	enabled := make(map[string]interface{}, len(medik8sparams.WAOAllComponents))
	for _, component := range medik8sparams.WAOAllComponents {
		enabled[component] = map[string]interface{}{"enabled": true}
	}

	k8sClient := waoClient(waoConfig(enabled))

	if err := DisableWAOComponents(
		context.Background(), k8sClient, medik8sparams.WAOAllComponents...); err != nil {
		t.Fatalf("DisableWAOComponents: %v", err)
	}

	for _, component := range medik8sparams.WAOAllComponents {
		if waoEnabled(t, k8sClient, component) {
			t.Errorf("operand %s is still enabled", component)
		}
	}
}

func TestSetWAOComponentsRejectsBadInput(t *testing.T) {
	k8sClient := waoClient(waoConfig(nil))

	for name, call := range map[string]func() error{
		"unknown operand": func() error {
			return EnableWAOComponents(context.Background(), k8sClient, "nodeHealthChecks")
		},
		"no operand": func() error {
			return EnableWAOComponents(context.Background(), k8sClient)
		},
		"no client": func() error {
			return EnableWAOComponents(context.Background(), nil, medik8sparams.WAOComponentNHC)
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestSetWAOComponentsReportsMissingSingleton(t *testing.T) {
	err := EnableWAOComponents(context.Background(), waoClient(), medik8sparams.WAOComponentNHC)
	if err == nil {
		t.Fatal("expected an error when the configuration does not exist")
	}
}

func TestWAOComponentRetainedResources(t *testing.T) {
	retained := "self-node-remediation.medik8s.io/v1alpha1 SelfNodeRemediationConfig ns/self-node-remediation-config"
	config := waoConfig(nil)
	config.Object["status"] = map[string]interface{}{"components": []interface{}{
		map[string]interface{}{
			"name":              medik8sparams.WAOComponentSNR,
			"phase":             medik8sparams.WAOPhaseDisableIncomplete,
			"retainedResources": []interface{}{retained},
		},
		map[string]interface{}{"name": medik8sparams.WAOComponentMDR, "phase": medik8sparams.WAOPhaseDisabled},
	}}

	for component, want := range map[string][]string{
		medik8sparams.WAOComponentSNR: {retained},
		medik8sparams.WAOComponentMDR: nil,
		medik8sparams.WAOComponentNHC: nil,
	} {
		got, err := WAOComponentRetainedResources(config, component)
		if err != nil {
			t.Fatalf("WAOComponentRetainedResources(%s): %v", component, err)
		}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("operand %s retained = %v, want %v", component, got, want)
		}
	}
}

func TestWAOOperandsForLabelFilter(t *testing.T) {
	for name, testCase := range map[string]struct {
		filter string
		want   []string
	}{
		"single operator with tier": {"operator:far && tier:smoke", []string{medik8sparams.WAOComponentFAR}},
		"two operators or": {
			"operator:far || operator:nhc",
			[]string{medik8sparams.WAOComponentFAR, medik8sparams.WAOComponentNHC},
		},
		"negated operator ignored": {"!operator:far && operator:nhc", []string{medik8sparams.WAOComponentNHC}},
		"duplicate deduplicated":   {"operator:sbr && operator:sbr", []string{medik8sparams.WAOComponentSBR}},
		"no operator term":         {"tier:smoke && frequency:nightly", nil},
		"empty filter":             {"", nil},
		"wao has no operand":       {"operator:wao", nil},
		"interop has no operand":   {"operator:interop", nil},
	} {
		if got := WAOOperandsForLabelFilter(testCase.filter); !reflect.DeepEqual(got, testCase.want) {
			t.Errorf("%s: WAOOperandsForLabelFilter(%q) = %v, want %v", name, testCase.filter, got, testCase.want)
		}
	}
}

func TestEnsureWAOOperandsEnabledNoOpWithoutOperatorLabel(t *testing.T) {
	t.Setenv(ECOTestLabelsEnvVar, "tier:smoke")
	k8sClient := waoClient(waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentFAR: map[string]interface{}{"enabled": false},
	}))
	operands, err := EnsureWAOOperandsEnabled(context.Background(), k8sClient, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureWAOOperandsEnabled: %v", err)
	}

	if operands != nil {
		t.Errorf("operands = %v, want nil", operands)
	}

	if waoEnabled(t, k8sClient, medik8sparams.WAOComponentFAR) {
		t.Error("fenceAgentsRemediation was enabled despite no operator label in the filter")
	}
}

func TestEnsureWAOOperandsEnabledNoOpOnPreWAOCluster(t *testing.T) {
	t.Setenv(ECOTestLabelsEnvVar, "operator:far")
	operands, err := EnsureWAOOperandsEnabled(
		context.Background(), waoClient(), time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureWAOOperandsEnabled: %v", err)
	}

	if operands != nil {
		t.Errorf("operands = %v, want nil on a pre-WAO cluster", operands)
	}
}

func TestEnsureWAOOperandsEnabledIdempotentWhenAvailable(t *testing.T) {
	t.Setenv(ECOTestLabelsEnvVar, "operator:far && tier:smoke")
	config := waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentFAR: map[string]interface{}{"enabled": true},
	})
	config.Object["status"] = map[string]interface{}{"components": []interface{}{
		map[string]interface{}{"name": medik8sparams.WAOComponentFAR, "phase": medik8sparams.WAOPhaseAvailable},
	}}
	operands, err := EnsureWAOOperandsEnabled(
		context.Background(), waoClient(config), time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureWAOOperandsEnabled: %v", err)
	}

	if !reflect.DeepEqual(operands, []string{medik8sparams.WAOComponentFAR}) {
		t.Errorf("operands = %v, want [%s]", operands, medik8sparams.WAOComponentFAR)
	}
}

func TestEnsureWAOOperandsEnabledEnablesDisabledOperand(t *testing.T) {
	t.Setenv(ECOTestLabelsEnvVar, "operator:far")
	config := waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentFAR: map[string]interface{}{"enabled": false},
	})
	// WAO reports Available once the manager is up; the precondition enables the operand and then
	// observes this phase. The fake client does not reconcile, so the phase is seeded here.
	config.Object["status"] = map[string]interface{}{"components": []interface{}{
		map[string]interface{}{"name": medik8sparams.WAOComponentFAR, "phase": medik8sparams.WAOPhaseAvailable},
	}}
	k8sClient := waoClient(config)
	operands, err := EnsureWAOOperandsEnabled(context.Background(), k8sClient, time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureWAOOperandsEnabled: %v", err)
	}

	if !reflect.DeepEqual(operands, []string{medik8sparams.WAOComponentFAR}) {
		t.Errorf("operands = %v, want [%s]", operands, medik8sparams.WAOComponentFAR)
	}

	if !waoEnabled(t, k8sClient, medik8sparams.WAOComponentFAR) {
		t.Error("fenceAgentsRemediation was not enabled")
	}
}

func TestWAOComponentPhase(t *testing.T) {
	config := waoConfig(nil)
	config.Object["status"] = map[string]interface{}{"components": []interface{}{
		map[string]interface{}{"name": medik8sparams.WAOComponentSNR, "phase": medik8sparams.WAOPhaseAvailable},
		map[string]interface{}{"name": medik8sparams.WAOComponentNHC, "phase": medik8sparams.WAOPhaseProgressing},
	}}

	for component, want := range map[string]string{
		medik8sparams.WAOComponentSNR: medik8sparams.WAOPhaseAvailable,
		medik8sparams.WAOComponentNHC: medik8sparams.WAOPhaseProgressing,
		medik8sparams.WAOComponentSBR: "",
	} {
		phase, err := WAOComponentPhase(config, component)
		if err != nil {
			t.Fatalf("WAOComponentPhase(%s): %v", component, err)
		}

		if phase != want {
			t.Errorf("operand %s phase = %q, want %q", component, phase, want)
		}
	}
}
