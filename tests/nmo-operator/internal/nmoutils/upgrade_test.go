package nmoutils

import (
	"context"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func maintenanceFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name": "owned", "uid": "original",
			"labels": map[string]interface{}{helpers.FBCRunLabel: "run"},
		},
		"spec": map[string]interface{}{"nodeName": "worker", "reason": "persist me"},
		"status": map[string]interface{}{
			"phase": "Succeeded", "drainProgress": int64(100),
			"lastUpdate": "2026-10-06T10:00:05Z", "lastError": "old transient drain error",
		},
	}}
}

func TestVerifyConfigurationAndOwnership(t *testing.T) {
	original := maintenanceFixture()
	spec, _, _ := unstructured.NestedMap(original.Object, "spec")
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		valid  bool
	}{
		{"same complete spec", func(*unstructured.Unstructured) {}, true},
		{"status only", func(o *unstructured.Unstructured) { delete(o.Object, "status") }, true},
		{"recreated", func(o *unstructured.Unstructured) { o.SetUID("replacement") }, false},
		{"changed reason", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "different", "spec", "reason")
		}, false},
		{"changed node", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "different", "spec", "nodeName")
		}, false},
		{"missing spec", func(o *unstructured.Unstructured) { delete(o.Object, "spec") }, false},
		{"deleting", func(o *unstructured.Unstructured) { now := metav1.Now(); o.SetDeletionTimestamp(&now) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := original.DeepCopy()
			test.mutate(object)
			err := helpers.VerifyConfigurationPreserved(context.Background(), nil, object, "original", spec)
			if (err == nil) != test.valid {
				t.Fatalf("VerifyConfiguration() = %v, want valid %t", err, test.valid)
			}
		})
	}
	if err := VerifyOwned(original, "original", "run", "worker"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ token, node string }{{"foreign", "worker"}, {"run", "replacement"}} {
		if err := VerifyOwned(original, "original", test.token, test.node); err == nil {
			t.Fatal("unowned maintenance accepted")
		}
	}
}

func TestVerifyMaintenanceFreshness(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-10-06T10:00:00Z")
	for _, test := range []struct {
		name, field string
		value       interface{}
		valid       bool
	}{
		{"fresh successful retry with retained lastError", "phase", "Succeeded", true},
		{"running", "phase", "Running", false},
		{"failed", "phase", "Failed", false},
		{"partial drain", "drainProgress", int64(50), false},
		{"stale status", "lastUpdate", "2026-10-06T09:59:59Z", false},
		{"same second reconciliation", "lastUpdate", "2026-10-06T10:00:00Z", true},
		{"invalid timestamp", "lastUpdate", "", false},
		{"pending pods", "pendingPods", []interface{}{"pod"}, false},
		{"pending references", "pendingPodsRefs", []interface{}{map[string]interface{}{"name": "pod"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := maintenanceFixture()
			if err := unstructured.SetNestedField(object.Object, test.value, "status", test.field); err != nil {
				t.Fatal(err)
			}
			if err := VerifyMaintenance(object, since); (err == nil) != test.valid {
				t.Fatalf("VerifyMaintenance() = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func TestVerifyWorkerMaintenanceAndRecovery(t *testing.T) {
	original := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker", UID: "original", Labels: map[string]string{"node-role.kubernetes.io/worker": ""},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	for _, test := range []struct {
		name        string
		maintenance bool
		mutate      func(*corev1.Node)
		valid       bool
	}{
		{"normal worker", false, func(*corev1.Node) {}, true},
		{"complete maintenance", true, func(n *corev1.Node) {
			n.Spec.Unschedulable = true
			n.Spec.Taints = []corev1.Taint{{Key: nmoparams.DrainTaintKey, Effect: corev1.TaintEffectNoSchedule}}
			n.Labels[ExcludeRemediationLabel] = "true"
		}, true},
		{"replacement node", false, func(n *corev1.Node) { n.UID = "replacement" }, false},
		{"control plane worker", false, func(n *corev1.Node) {
			n.Labels["node-role.kubernetes.io/control-plane"] = ""
		}, false},
		{"master worker", false, func(n *corev1.Node) { n.Labels["node-role.kubernetes.io/master"] = "" }, false},
		{"not Ready", false, func(n *corev1.Node) { n.Status.Conditions = nil }, false},
		{"not a worker", false, func(n *corev1.Node) { n.Labels = nil }, false},
		{"still cordoned", false, func(n *corev1.Node) { n.Spec.Unschedulable = true }, false},
		{"still drain tainted", false, func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: nmoparams.DrainTaintKey, Effect: corev1.TaintEffectNoSchedule}}
		}, false},
		{"still excluded", false, func(n *corev1.Node) { n.Labels[ExcludeRemediationLabel] = "true" }, false},
		{"missing maintenance state", true, func(*corev1.Node) {}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := original.DeepCopy()
			test.mutate(node)
			if err := VerifyNode(node, "original", test.maintenance); (err == nil) != test.valid {
				t.Fatalf("VerifyNode() = %v, want valid %t", err, test.valid)
			}
		})
	}
}
