package helpers

import (
	"context"
	"reflect"
	"strings"
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

// waoConfigWithStatus builds the singleton with the given intent and observed phase per operand.
func waoConfigWithStatus(enabled map[string]bool, phases map[string]string) *unstructured.Unstructured {
	components := make(map[string]interface{}, len(enabled))
	for component, value := range enabled {
		components[component] = map[string]interface{}{"enabled": value}
	}

	statuses := make([]interface{}, 0, len(phases))
	for component, phase := range phases {
		statuses = append(statuses, map[string]interface{}{
			"name": component, "phase": phase, "admissionReady": phase != medik8sparams.WAOPhaseDegraded,
		})
	}

	config := waoConfig(components)
	config.Object["status"] = map[string]interface{}{"components": statuses}

	return config
}

func TestEnsureWAOComponentsAvailable(t *testing.T) {
	const timeout, interval = 50 * time.Millisecond, 10 * time.Millisecond

	t.Run("no singleton is a cluster without WAO", func(t *testing.T) {
		err := EnsureWAOComponentsAvailable(
			context.Background(), waoClient(), timeout, interval, medik8sparams.WAOComponentFAR)
		if err != nil {
			t.Fatalf("EnsureWAOComponentsAvailable: %v", err)
		}
	})

	t.Run("an Available operand is not patched", func(t *testing.T) {
		k8sClient := waoClient(waoConfigWithStatus(
			map[string]bool{medik8sparams.WAOComponentFAR: true},
			map[string]string{medik8sparams.WAOComponentFAR: medik8sparams.WAOPhaseAvailable}))
		before, err := GetWAOConfig(context.Background(), k8sClient)
		if err != nil {
			t.Fatalf("GetWAOConfig: %v", err)
		}

		err = EnsureWAOComponentsAvailable(
			context.Background(), k8sClient, timeout, interval, medik8sparams.WAOComponentFAR)
		if err != nil {
			t.Fatalf("EnsureWAOComponentsAvailable: %v", err)
		}

		after, err := GetWAOConfig(context.Background(), k8sClient)
		if err != nil {
			t.Fatalf("GetWAOConfig: %v", err)
		}

		if before.GetResourceVersion() != after.GetResourceVersion() {
			t.Errorf("resourceVersion changed from %s to %s; the precondition patched a correct cluster",
				before.GetResourceVersion(), after.GetResourceVersion())
		}
	})

	t.Run("a Disabled operand is enabled and the wait names it", func(t *testing.T) {
		k8sClient := waoClient(waoConfigWithStatus(
			map[string]bool{medik8sparams.WAOComponentNHC: false},
			map[string]string{medik8sparams.WAOComponentNHC: medik8sparams.WAOPhaseDisabled}))

		// No controller changes the phase of the fake object, so the wait must end with an error.
		err := EnsureWAOComponentsAvailable(
			context.Background(), k8sClient, timeout, interval, medik8sparams.WAOComponentNHC)
		if err == nil || !strings.Contains(err.Error(), medik8sparams.WAOComponentNHC) {
			t.Fatalf("EnsureWAOComponentsAvailable error = %v, want a timeout that names the operand", err)
		}

		if !waoEnabled(t, k8sClient, medik8sparams.WAOComponentNHC) {
			t.Error("nodeHealthCheck was not enabled")
		}
	})
}

func TestWAOComponentAdmissionReady(t *testing.T) {
	config := waoConfigWithStatus(nil, map[string]string{
		medik8sparams.WAOComponentFAR: medik8sparams.WAOPhaseDisabled,
		medik8sparams.WAOComponentNHC: medik8sparams.WAOPhaseDegraded,
	})

	for component, want := range map[string]bool{
		medik8sparams.WAOComponentFAR: true,
		medik8sparams.WAOComponentNHC: false,
		medik8sparams.WAOComponentMDR: false,
	} {
		got, err := WAOComponentAdmissionReady(config, component)
		if err != nil {
			t.Fatalf("WAOComponentAdmissionReady(%s): %v", component, err)
		}

		if got != want {
			t.Errorf("WAOComponentAdmissionReady(%s) = %v, want %v", component, got, want)
		}
	}
}
