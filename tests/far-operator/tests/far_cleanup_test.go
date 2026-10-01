package tests

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/medik8s/system-tests/tests/far-operator/internal/farparams"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
)

func TestRemoveFARFinalizer(t *testing.T) {
	ctx := context.Background()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(farGVK)
	obj.SetName("worker")
	obj.SetNamespace(medik8sparams.OperatorNs)
	obj.SetFinalizers([]string{farRemediationFinalizer, "example.com/other"})
	k8sClient := fake.NewClientBuilder().WithObjects(obj).Build()

	if err := removeFARFinalizer(ctx, k8sClient, obj.GetName()); err != nil {
		t.Fatal(err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}

	if got := obj.GetFinalizers(); len(got) != 1 || got[0] != "example.com/other" {
		t.Fatalf("unrelated finalizer was not preserved: %v", got)
	}

	if err := removeFARFinalizer(ctx, k8sClient, "absent"); err != nil {
		t.Fatalf("absent CR cleanup failed: %v", err)
	}
}

func TestRestoreFARNodeScheduling(t *testing.T) {
	ctx := context.Background()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	savedTaint := corev1.Taint{Key: corev1.TaintNodeOutOfService, Effect: corev1.TaintEffectNoExecute}
	otherTaint := corev1.Taint{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}
	original := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{savedTaint}}}
	node := original.DeepCopy()
	node.Spec.Unschedulable = true
	node.Spec.Taints = append(node.Spec.Taints, otherTaint,
		corev1.Taint{Key: farparams.FARNoScheduleTaintKey, Effect: corev1.TaintEffectNoSchedule})
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

	if err := restoreFARNodeScheduling(ctx, k8sClient, original); err != nil {
		t.Fatal(err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}

	if node.Spec.Unschedulable || len(node.Spec.Taints) != 2 ||
		node.Spec.Taints[0] != savedTaint || node.Spec.Taints[1] != otherTaint {
		t.Fatalf("unexpected restored scheduling state: %+v", node.Spec)
	}
}
