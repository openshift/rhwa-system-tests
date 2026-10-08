package helpers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	diagnosticTestTailLines = 100
	diagnosticTestTimeout   = 20 * time.Millisecond
)

var diagnosticTestGVK = schema.GroupVersionKind{Group: "test.medik8s.io", Version: "v1", Kind: "Remediation"}

type diagnosticTestClient struct {
	client.Client
	corev1client.CoreV1Interface
}

func diagnosticTestCR(name, namespace string, status interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"password": "do-not-log-this"},
	}}
	obj.SetGroupVersionKind(diagnosticTestGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.SetUID("test-uid")

	if status != nil {
		obj.Object["status"] = status
	}

	return obj
}

func diagnosticTestLogger(output *bytes.Buffer) func(string, ...interface{}) {
	return func(format string, args ...interface{}) { fmt.Fprintf(output, format, args...) }
}

func TestDiagnosticResourcesFor(t *testing.T) {
	resources := DiagnosticResourcesFor(diagnosticTestGVK, "operators", "worker-0", "worker-1")
	want := []DiagnosticResource{
		{GVK: diagnosticTestGVK, Key: client.ObjectKey{Name: "worker-0", Namespace: "operators"}},
		{GVK: diagnosticTestGVK, Key: client.ObjectKey{Name: "worker-1", Namespace: "operators"}},
	}

	if !reflect.DeepEqual(resources, want) {
		t.Errorf("unexpected resources: got %v, want %v", resources, want)
	}

	if resources := DiagnosticResourcesFor(diagnosticTestGVK, ""); len(resources) != 0 {
		t.Errorf("expected no resources without names, got %v", resources)
	}
}

func TestLogCRStatus(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		status    interface{}
		want      string
	}{
		{
			name: "conditions", namespace: "operators",
			status: map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
				"type": "Succeeded", "status": "False", "reason": "Stalled", "message": "fencing pending",
			}}},
			want: `type=Succeeded status=False reason=Stalled message="fencing pending"`,
		},
		{
			name: "cluster-scoped-maintenance",
			status: map[string]interface{}{
				"phase": "Running", "drainProgress": int64(0), "lastError": "PDB prevents eviction",
				"pendingPods": []interface{}{"workload"},
			},
			want: `"lastError":"PDB prevents eviction"`,
		},
		{name: "no-status", want: "has no status yet"},
		{name: "malformed-status", status: "invalid", want: "WARNING: could not read"},
		{
			name:   "malformed-condition",
			status: map[string]interface{}{"conditions": []interface{}{"invalid"}},
			want:   "WARNING: malformed condition",
		},
		{
			name: "malformed-conditions", status: map[string]interface{}{"conditions": "invalid"},
			want: "WARNING: could not read",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			obj := diagnosticTestCR(testCase.name, testCase.namespace, testCase.status)
			k8sClient := fake.NewClientBuilder().WithRuntimeObjects(obj).Build()
			var output bytes.Buffer
			LogCRStatus(context.Background(), k8sClient, diagnosticTestGVK,
				client.ObjectKeyFromObject(obj), diagnosticTestLogger(&output))

			if !strings.Contains(output.String(), testCase.want) {
				t.Fatalf("missing %q in diagnostics: %s", testCase.want, &output)
			}

			if strings.Contains(output.String(), "do-not-log-this") {
				t.Fatal("diagnostics leaked the CR spec")
			}
		})
	}
}

func TestLogCRStatusMissingAndEmptyNames(t *testing.T) {
	var output bytes.Buffer
	k8sClient := fake.NewClientBuilder().Build()
	LogCRStatus(context.Background(), k8sClient, diagnosticTestGVK,
		client.ObjectKey{Name: "missing"}, diagnosticTestLogger(&output))

	if !strings.Contains(output.String(), "not present") ||
		strings.Contains(output.String(), "WARNING") {
		t.Fatalf("missing CR should be reported as not present: %s", &output)
	}

	output.Reset()
	LogCRStatus(context.Background(), k8sClient, diagnosticTestGVK,
		client.ObjectKey{}, diagnosticTestLogger(&output))

	if output.Len() != 0 {
		t.Fatalf("unnamed CR should be skipped: %s", &output)
	}
}

func newDiagnosticTestClient(
	t *testing.T, handler http.HandlerFunc, holder *string,
) (diagnosticTestClient, ControllerDiagnostics) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	coreClient, err := corev1client.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-election", Namespace: "operators"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: holder},
	}
	// The standby shares the leader's node: logs must still target the exact lease holder.
	standby := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standby", Namespace: "operators"},
		Spec:       corev1.PodSpec{NodeName: "shared-node"},
	}
	leader := standby.DeepCopy()
	leader.Name = "leader"
	apiClient := diagnosticTestClient{
		Client:          fake.NewClientBuilder().WithScheme(scheme).WithObjects(lease, standby, leader).Build(),
		CoreV1Interface: coreClient,
	}
	controller := ControllerDiagnostics{
		Namespace: "operators", LeaseName: lease.Name, ContainerName: "manager", TailLines: diagnosticTestTailLines,
	}

	return apiClient, controller
}

func TestLogActiveControllerLogs(t *testing.T) {
	identity := "leader_uuid"
	requested := false
	apiClient, controller := newDiagnosticTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
		requested = true

		if request.URL.Path != "/api/v1/namespaces/operators/pods/leader/log" ||
			request.URL.Query().Get("container") != "manager" ||
			request.URL.Query().Get("tailLines") != strconv.Itoa(diagnosticTestTailLines) {
			t.Errorf("unexpected pod log request: %s", request.URL)
		}

		fmt.Fprint(writer, "reconcile stalled\n")
	}, &identity)
	var output bytes.Buffer
	LogActiveControllerLogs(context.Background(), apiClient, controller, diagnosticTestLogger(&output))

	if !requested || !strings.Contains(output.String(), "holder=leader_uuid") ||
		!strings.Contains(output.String(), "reconcile stalled") {
		t.Fatalf("missing exact leader logs: %s", &output)
	}
}

func TestLogActiveControllerLogsErrors(t *testing.T) {
	identity := "leader_uuid"
	apiClient, controller := newDiagnosticTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}, &identity)
	var output bytes.Buffer
	LogActiveControllerLogs(context.Background(), apiClient, controller, diagnosticTestLogger(&output))

	if !strings.Contains(output.String(), "WARNING: could not fetch controller pod leader logs") {
		t.Fatalf("log endpoint failure did not produce a warning: %s", &output)
	}

	output.Reset()
	controller.LeaseName = "missing-lease"
	LogActiveControllerLogs(context.Background(), apiClient, controller, diagnosticTestLogger(&output))

	if !strings.Contains(output.String(), "WARNING: could not resolve controller lease") {
		t.Fatalf("missing lease did not produce a warning: %s", &output)
	}
}

func TestLogActiveControllerLogsInvalidHolder(t *testing.T) {
	for _, holder := range []*string{nil, new(string)} {
		apiClient, controller := newDiagnosticTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("log endpoint should not be called without a valid leader")
		}, holder)
		var output bytes.Buffer
		LogActiveControllerLogs(context.Background(), apiClient, controller, diagnosticTestLogger(&output))

		if !strings.Contains(output.String(), "WARNING: could not resolve controller lease") {
			t.Fatalf("invalid holder did not produce a warning: %s", &output)
		}
	}
}

func TestLogActiveControllerLogsTimeout(t *testing.T) {
	identity := "leader_uuid"
	apiClient, controller := newDiagnosticTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}, &identity)
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticTestTimeout)
	defer cancel()
	var output bytes.Buffer
	LogActiveControllerLogs(ctx, apiClient, controller, diagnosticTestLogger(&output))

	if !strings.Contains(output.String(), "WARNING: could not fetch controller pod leader logs") {
		t.Fatalf("log timeout did not produce a warning: %s", &output)
	}
}

func TestLogRemediationDiagnosticsIndependentRequests(t *testing.T) {
	identity := "leader_uuid"
	requested := false
	apiClient, controller := newDiagnosticTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requested = true
		fmt.Fprint(w, "controller evidence")
	}, &identity)
	obj := diagnosticTestCR("present", "operators", map[string]interface{}{"phase": "Processing"})

	if err := apiClient.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}

	watchClient, ok := apiClient.Client.(client.WithWatch)
	if !ok {
		t.Fatal("fake client must support watch")
	}

	apiClient.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
		Get: func(ctx context.Context, k8sClient client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if ctx.Err() != nil {
				t.Errorf("diagnostic request inherited spec cancellation: %v", ctx.Err())
			}

			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) > DiagnosticsRequestTimeout {
				t.Error("diagnostic request has no bounded timeout")
			}

			return k8sClient.Get(ctx, key, obj, opts...)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	LogRemediationDiagnostics(ctx, apiClient, []DiagnosticResource{
		{GVK: diagnosticTestGVK, Key: client.ObjectKey{Name: "missing", Namespace: "operators"}},
		{GVK: diagnosticTestGVK, Key: client.ObjectKeyFromObject(obj)},
		{GVK: diagnosticTestGVK, Key: client.ObjectKey{}},
	}, controller, diagnosticTestLogger(&output))

	for _, want := range []string{
		"Remediation operators/missing not present", `"phase":"Processing"`, "controller evidence",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q in diagnostics: %s", want, &output)
		}
	}

	if !requested {
		t.Fatal("CR fetch failure prevented controller log collection")
	}
}
