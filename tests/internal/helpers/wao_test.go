package helpers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// waoStatus sets status.components for the given operand phases.
func waoStatus(config *unstructured.Unstructured, phases map[string]string) *unstructured.Unstructured {
	statuses := make([]interface{}, 0, len(phases))
	for component, phase := range phases {
		statuses = append(statuses, map[string]interface{}{"name": component, "phase": phase})
	}
	config.Object["status"] = map[string]interface{}{"components": statuses}

	return config
}

func TestEnsureWAOComponentsLeavesAnAvailableOperandAlone(t *testing.T) {
	config := waoStatus(waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentSNR: map[string]interface{}{
			"enabled": true,
			"manager": map[string]interface{}{"replicas": int64(2)},
		},
	}), map[string]string{medik8sparams.WAOComponentSNR: medik8sparams.WAOPhaseAvailable})
	k8sClient := waoClient(config)
	before, err := GetWAOConfig(context.Background(), k8sClient)
	if err != nil {
		t.Fatalf("GetWAOConfig: %v", err)
	}

	if ensureErr := EnsureWAOComponents(
		context.Background(), k8sClient, medik8sparams.WAOComponentSNR); ensureErr != nil {
		t.Fatalf("EnsureWAOComponents: %v", ensureErr)
	}

	after, err := GetWAOConfig(context.Background(), k8sClient)
	if err != nil {
		t.Fatalf("GetWAOConfig: %v", err)
	}
	// An operand that is already Available must not be patched: a bumped resourceVersion here
	// means a suite rerun restarted a manager its specs were about to use.
	if before.GetResourceVersion() != after.GetResourceVersion() {
		t.Errorf("configuration was patched for an already Available operand: resourceVersion %s -> %s",
			before.GetResourceVersion(), after.GetResourceVersion())
	}
}

func TestEnsureWAOComponentsReportsAPreWAOCluster(t *testing.T) {
	err := EnsureWAOComponents(context.Background(), waoClient(), medik8sparams.WAOComponentSNR)
	if !errors.Is(err, ErrWAONotInstalled) {
		t.Fatalf("EnsureWAOComponents error = %v, want one matching ErrWAONotInstalled", err)
	}
}

func TestEnsureWAOComponentsRejectsNoOperand(t *testing.T) {
	if err := EnsureWAOComponents(context.Background(), waoClient()); err == nil {
		t.Fatal("expected an error when no operand is named")
	}
}

func TestWAORunRequestedReadsTheLabelFilter(t *testing.T) {
	for filter, want := range map[string]bool{
		"operator:wao":                                 true,
		"operator:wao || operator:snr":                 true,
		"operator:snr||operator:wao":                   true,
		"(operator:wao || operator:snr) && tier:smoke": true,
		"operator:snr":                                 false,
		"":                                             false,
		"!operator:wao":                                false,
		"operator:snr && !operator:wao":                false,
		"operator:waox":                                false,
		"operator:snr && tier:smoke && !tier:upgrade": false,
	} {
		t.Setenv("ECO_TEST_LABELS", "")

		if got := WAORunRequested(filter); got != want {
			t.Errorf("WAORunRequested(%q) = %v, want %v", filter, got, want)
		}
	}
}

func TestWAORunRequestedPrefersTheEnvironment(t *testing.T) {
	// scripts/test-runner.sh turns ECO_TEST_LABELS into --label-filter, so the two normally
	// agree; when a run sets only the environment, that is the filter.
	t.Setenv("ECO_TEST_LABELS", "operator:wao || operator:snr")

	if !WAORunRequested("") {
		t.Error("WAORunRequested ignored ECO_TEST_LABELS")
	}

	t.Setenv("ECO_TEST_LABELS", "operator:snr")

	if WAORunRequested("operator:wao") {
		t.Error("WAORunRequested used the ginkgo filter while ECO_TEST_LABELS was set")
	}
}

// snrcRef is a stand-in for an operand's default configuration CR.
var snrcRef = WAOOperandConfigRef{
	GVK: schema.GroupVersionKind{
		Group: "self-node-remediation.medik8s.io", Version: "v1alpha1", Kind: "SelfNodeRemediationConfig",
	},
	Namespace: medik8sparams.OperatorNs,
	Name:      "self-node-remediation-config",
}

// readyOperand builds the cluster state a fully reconciled SNR operand leaves behind.
func readyOperand() []client.Object {
	config := waoStatus(waoConfig(map[string]interface{}{
		medik8sparams.WAOComponentSNR: map[string]interface{}{"enabled": true},
	}), map[string]string{medik8sparams.WAOComponentSNR: medik8sparams.WAOPhaseAvailable})
	manager := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      medik8sparams.WAOOperandDeployments[medik8sparams.WAOComponentSNR][0],
			Namespace: medik8sparams.OperatorNs,
		},
		Status: appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2},
	}
	agents := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "self-node-remediation-ds", Namespace: medik8sparams.OperatorNs},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 6, NumberReady: 6, UpdatedNumberScheduled: 6,
		},
	}
	snrc := &unstructured.Unstructured{}
	snrc.SetGroupVersionKind(snrcRef.GVK)
	snrc.SetNamespace(snrcRef.Namespace)
	snrc.SetName(snrcRef.Name)

	return []client.Object{config, manager, agents, snrc}
}

func readyScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(WAOConfigGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(WAOConfigGVK.GroupVersion().WithKind(WAOConfigGVK.Kind+"List"),
		&unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(snrcRef.GVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(snrcRef.GVK.GroupVersion().WithKind(snrcRef.GVK.Kind+"List"),
		&unstructured.UnstructuredList{})
	_ = appsv1.AddToScheme(scheme)

	return scheme
}

func TestEnsureWAOOperandReadyAcceptsAFullyReconciledOperand(t *testing.T) {
	k8sClient := fake.NewClientBuilder().WithScheme(readyScheme()).WithObjects(readyOperand()...).Build()
	err := EnsureWAOOperandReady(context.Background(), k8sClient, WAOOperandReadiness{
		Component:  medik8sparams.WAOComponentSNR,
		Configs:    []WAOOperandConfigRef{snrcRef},
		DaemonSets: []string{"self-node-remediation-ds"},
	})
	if err != nil {
		t.Fatalf("EnsureWAOOperandReady on a ready operand: %v", err)
	}
}

// The race this exists to close: WAO reports Available, but the operand's controller has not
// yet written its configuration or its agents. Each wait must reject that state rather than
// let a spec run against a half-built operand.
func TestEnsureWAOOperandReadyRejectsAHalfReconciledOperand(t *testing.T) {
	t.Parallel()

	for name, drop := range map[string]string{
		"no default configuration": "config",
		"no agent DaemonSet":       "daemonset",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var objects []client.Object

			for _, object := range readyOperand() {
				if drop == "config" && object.GetObjectKind().GroupVersionKind() == snrcRef.GVK {
					continue
				}

				if _, isAgents := object.(*appsv1.DaemonSet); isAgents && drop == "daemonset" {
					continue
				}

				objects = append(objects, object)
			}

			k8sClient := fake.NewClientBuilder().WithScheme(readyScheme()).WithObjects(objects...).Build()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()

			err := EnsureWAOOperandReady(ctx, k8sClient, WAOOperandReadiness{
				Component:  medik8sparams.WAOComponentSNR,
				Configs:    []WAOOperandConfigRef{snrcRef},
				DaemonSets: []string{"self-node-remediation-ds"},
			})
			if err == nil {
				t.Fatalf("EnsureWAOOperandReady accepted an operand with %s", name)
			}
		})
	}
}

// A DaemonSet reporting zero desired pods is what a just-created one looks like; treating it
// as ready would reintroduce the race one layer down.
func TestEnsureWAOOperandReadyRejectsAnUnscheduledDaemonSet(t *testing.T) {
	var objects []client.Object

	for _, object := range readyOperand() {
		if agents, isAgents := object.(*appsv1.DaemonSet); isAgents {
			agents.Status = appsv1.DaemonSetStatus{}
		}

		objects = append(objects, object)
	}

	k8sClient := fake.NewClientBuilder().WithScheme(readyScheme()).WithObjects(objects...).Build()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := EnsureWAOOperandReady(ctx, k8sClient, WAOOperandReadiness{
		Component:  medik8sparams.WAOComponentSNR,
		DaemonSets: []string{"self-node-remediation-ds"},
	})
	if err == nil {
		t.Fatal("EnsureWAOOperandReady accepted a DaemonSet with no scheduled pods")
	}
}

// A DaemonSet whose status still describes the previous generation is mid-rollout: ready and
// up to date, but about to replace its pods. Accepting it is what let the must-gather spec
// capture pod names that no longer existed thirteen seconds later.
func TestWaitForDaemonSetStableRejectsAStaleGeneration(t *testing.T) {
	agents := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "self-node-remediation-ds", Namespace: medik8sparams.OperatorNs, Generation: 8,
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 7, DesiredNumberScheduled: 6, NumberReady: 6, UpdatedNumberScheduled: 6,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(readyScheme()).WithObjects(agents).Build()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := WaitForDaemonSetStable(ctx, k8sClient, "self-node-remediation-ds"); err == nil {
		t.Fatal("WaitForDaemonSetStable accepted a DaemonSet whose status lags its generation")
	}
}

func TestWaitForDaemonSetStableRejectsUnavailablePods(t *testing.T) {
	agents := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "self-node-remediation-ds", Namespace: medik8sparams.OperatorNs, Generation: 3,
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 3, DesiredNumberScheduled: 6, NumberReady: 6,
			UpdatedNumberScheduled: 6, NumberUnavailable: 1,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(readyScheme()).WithObjects(agents).Build()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := WaitForDaemonSetStable(ctx, k8sClient, "self-node-remediation-ds"); err == nil {
		t.Fatal("WaitForDaemonSetStable accepted a DaemonSet reporting an unavailable pod")
	}
}

func TestWaitForDaemonSetStableAcceptsASettledRollout(t *testing.T) {
	agents := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "self-node-remediation-ds", Namespace: medik8sparams.OperatorNs, Generation: 8,
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 8, DesiredNumberScheduled: 6, NumberReady: 6, UpdatedNumberScheduled: 6,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(readyScheme()).WithObjects(agents).Build()

	if err := WaitForDaemonSetStable(context.Background(), k8sClient, "self-node-remediation-ds"); err != nil {
		t.Fatalf("WaitForDaemonSetStable rejected a settled DaemonSet: %v", err)
	}
}
