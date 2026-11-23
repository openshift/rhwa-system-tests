package tests

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestBPNWorkerTopologyUsesAllWorkers(t *testing.T) {
	for _, scenario := range []struct {
		count int
		peers []int
	}{
		{count: 0},
		{count: 1},
		{count: 2, peers: []int{1}},
		{count: 5, peers: []int{1, 2, 3, 4}},
	} {
		t.Run(fmt.Sprintf("%d workers", scenario.count), func(t *testing.T) {
			workers := make([]corev1.Node, 0, scenario.count)
			for number := scenario.count; number > 0; number-- {
				workers = append(workers, bpnNewReadyLinuxWorker(fmt.Sprintf("worker-%d", number)))
			}
			nodes, err := bpnValidateAndSortWorkerNodes(workers)
			if scenario.count < 2 {
				if err == nil || len(nodes) != 0 {
					t.Fatalf("worker validation must fail for %d workers", scenario.count)
				}

				return
			}
			if err != nil || len(nodes) != scenario.count {
				t.Fatalf("worker validation got %d Nodes and error %v, want %d Nodes", len(nodes), err, scenario.count)
			}
			for index, node := range nodes {
				if name := fmt.Sprintf("worker-%d", index+1); node.initial.Name != name {
					t.Fatalf("worker %d = %s, want %s", index, node.initial.Name, name)
				}
			}
			fixture := &bpnFixture{nodes: nodes}
			if peers := fixture.peerIndices(); !slices.Equal(peers, scenario.peers) {
				t.Fatalf("peer indices = %v, want %v", peers, scenario.peers)
			}
		})
	}
}

func TestBPNWorkerTopologyRejectsIneligibleWorkers(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*corev1.Node)
	}{
		{"NotReady", func(node *corev1.Node) { node.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{"cordoned", func(node *corev1.Node) { node.Spec.Unschedulable = true }},
		{"non-Linux", func(node *corev1.Node) { node.Status.NodeInfo.OperatingSystem = "windows" }},
		{"control plane", func(node *corev1.Node) { node.Labels["node-role.kubernetes.io/control-plane"] = "" }},
		{"master", func(node *corev1.Node) { node.Labels["node-role.kubernetes.io/master"] = "" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			workers := []corev1.Node{
				bpnNewReadyLinuxWorker("worker-1"), bpnNewReadyLinuxWorker("worker-2"), bpnNewReadyLinuxWorker("worker-3"),
				bpnNewReadyLinuxWorker("invalid-worker"),
			}
			scenario.change(&workers[3])
			nodes, err := bpnValidateAndSortWorkerNodes(workers)
			if err == nil || !strings.Contains(err.Error(), "invalid-worker") || len(nodes) != 0 {
				t.Fatalf("worker validation must reject the entire topology and name the ineligible worker: %v", err)
			}
		})
	}
}

func bpnNewReadyLinuxWorker(name string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{OperatingSystem: "linux"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestBPNReadFixedSpecDurations(t *testing.T) {
	gomega.RegisterTestingT(t)
	for _, testCase := range []struct {
		name     string
		field    string
		expected string
		actual   interface{}
		wantFail bool
	}{
		{name: "outage grace", field: "totalOutageGracePeriod", expected: "60s", actual: "1m0s"},
		{name: "recovery period", field: "autonomousRebootRecoveryPeriod", expected: "1m", actual: "60s"},
		{name: "I/O timeout", field: "ioTimeout", expected: "15s", actual: "15000ms"},
		{name: "NHC healthy delay", field: "healthyDelay", expected: "1m", actual: "60s"},
		{name: "changed duration", field: "totalOutageGracePeriod", expected: "60s", actual: "61s", wantFail: true},
		{name: "invalid duration", field: "totalOutageGracePeriod", expected: "60s", actual: "invalid", wantFail: true},
		{
			name: "invalid expected duration", field: "totalOutageGracePeriod",
			expected: "invalid", actual: "invalid", wantFail: true,
		},
		{name: "wrong type", field: "totalOutageGracePeriod", expected: "60s", actual: int64(60), wantFail: true},
		{name: "missing duration", field: "totalOutageGracePeriod", expected: "60s", wantFail: true},
		{name: "same string", field: "sharedStorageClass", expected: "60s", actual: "60s"},
		{name: "changed string", field: "sharedStorageClass", expected: "60s", actual: "1m0s", wantFail: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			object := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage-based-remediation.medik8s.io/v1alpha1",
				"kind":       "StorageBasedRemediationConfig",
				"metadata":   map[string]interface{}{"name": "duration-test", "uid": "config-uid"},
				"spec":       map[string]interface{}{},
			}}
			if testCase.actual != nil {
				if err := unstructured.SetNestedField(object.Object, testCase.actual, "spec", testCase.field); err != nil {
					t.Fatal(err)
				}
			}
			resource := fake.NewSimpleDynamicClient(runtime.NewScheme(), object).Resource(schema.GroupVersionResource{
				Group: "storage-based-remediation.medik8s.io", Version: "v1alpha1", Resource: "storagebasedremediationconfigs",
			})
			fixture := &bpnFixture{ctx: context.Background(), cleaning: true}
			err := gomega.InterceptGomegaFailure(func() {
				fixture.readAndAssertStoredSpec(object, map[string]interface{}{testCase.field: testCase.expected}, resource)
			})
			if (err != nil) != testCase.wantFail {
				t.Fatalf("readAndAssertStoredSpec failure = %v, want failure = %t", err, testCase.wantFail)
			}
		})
	}
}
