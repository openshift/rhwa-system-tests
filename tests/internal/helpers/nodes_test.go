package helpers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	commonlabels "github.com/medik8s/common/pkg/labels"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func readyWorker(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{commonlabels.WorkerRole: ""}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		}}},
	}
}

func bastionPod(nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ssh-bastion-" + nodeName, Namespace: sshBastionNamespace,
			Labels: map[string]string{sshBastionPodLabel: sshBastionPodValue},
		},
		Spec: corev1.PodSpec{NodeName: nodeName},
	}
}

// selectionAttempts repeats the random selection so a lucky pick cannot hide
// a node that should have been excluded.
const selectionAttempts = 20

func TestSelectWorkerNodeAvoidsSSHBastionNode(t *testing.T) {
	k8sClient := fake.NewClientBuilder().WithObjects(
		readyWorker("worker-a"), readyWorker("worker-b"), bastionPod("worker-a"),
	).Build()

	for range selectionAttempts {
		node, err := SelectWorkerNode(context.Background(), k8sClient)
		if err != nil {
			t.Fatalf("SelectWorkerNode: %v", err)
		}

		if node.Name != "worker-b" {
			t.Fatalf("selected bastion node %s", node.Name)
		}
	}
}

func TestSelectWorkerNodeFallsBackToBastionNodeWhenOnlyChoice(t *testing.T) {
	k8sClient := fake.NewClientBuilder().WithObjects(
		readyWorker("worker-a"), readyWorker("worker-b"), bastionPod("worker-a"),
	).Build()
	node, err := SelectWorkerNode(context.Background(), k8sClient, "worker-b")
	if err != nil {
		t.Fatalf("SelectWorkerNode: %v", err)
	}

	if node.Name != "worker-a" {
		t.Fatalf("expected fallback to bastion node worker-a, got %s", node.Name)
	}
}

func TestSSHBastionNodesWithoutBastion(t *testing.T) {
	var k8sClient client.Client = fake.NewClientBuilder().Build()

	nodes, err := SSHBastionNodes(context.Background(), k8sClient)
	if err != nil {
		t.Fatalf("SSHBastionNodes returned error: %v", err)
	}

	if len(nodes) != 0 {
		t.Fatalf("expected no bastion nodes, got %v", nodes)
	}
}

func TestSelectWorkerNodeFailsWhenBastionLookupFails(t *testing.T) {
	listErr := fmt.Errorf("pods is forbidden")
	k8sClient := fake.NewClientBuilder().
		WithObjects(readyWorker("worker-a")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isPodList := list.(*corev1.PodList); isPodList {
					return listErr
				}

				return c.List(ctx, list, opts...)
			},
		}).Build()

	_, err := SelectWorkerNode(context.Background(), k8sClient)
	if err == nil || !strings.Contains(err.Error(), listErr.Error()) {
		t.Fatalf("expected bastion lookup error, got %v", err)
	}
}

func TestLogNodeStateReportsCordonTaintsAndBootID(t *testing.T) {
	node := readyWorker("worker-a")
	node.Spec.Unschedulable = true
	node.Spec.Taints = []corev1.Taint{{
		Key: corev1.TaintNodeOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute,
	}}
	node.Status.NodeInfo.BootID = "boot-1"
	k8sClient := fake.NewClientBuilder().WithObjects(node).Build()

	var out strings.Builder

	LogNodeState(context.Background(), k8sClient, func(format string, args ...interface{}) {
		fmt.Fprintf(&out, format, args...)
	}, "worker-a", "missing-node", "")

	for _, want := range []string{
		"Node worker-a: Ready=True", "unschedulable=true", "bootID=boot-1",
		corev1.TaintNodeOutOfService + "=nodeshutdown:NoExecute", "Node missing-node: get failed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}
