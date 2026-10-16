package helpers

import (
	"context"
	"fmt"
	"math/rand"

	commonlabels "github.com/medik8s/common/pkg/labels"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsNodeReady returns true if the node has a Ready condition with status True.
func IsNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}

	return false
}

// pickRandomReadyNode filters candidates to Ready, schedulable nodes not in the
// excludeNodes list and returns one at random, or nil if none qualify.
// Randomization avoids deterministic reuse of the same node across sequential
// destructive tests.
func pickRandomReadyNode(candidates []corev1.Node, excludeNodes ...string) *corev1.Node {
	excluded := make(map[string]bool, len(excludeNodes))
	for _, name := range excludeNodes {
		excluded[name] = true
	}

	var eligible []corev1.Node

	for i := range candidates {
		node := &candidates[i]

		if excluded[node.Name] || node.Spec.Unschedulable {
			continue
		}

		if IsNodeReady(node) {
			eligible = append(eligible, *node)
		}
	}

	if len(eligible) == 0 {
		return nil
	}

	return &eligible[rand.Intn(len(eligible))]
}

const (
	// sshBastionNamespace and sshBastionPodLabel identify the in-cluster SSH
	// bastion deployed by the ssh-bastion step-registry ref.
	sshBastionNamespace = "test-ssh-bastion"
	sshBastionPodLabel  = "run"
	sshBastionPodValue  = "ssh-bastion"
)

// SSHBastionNodes returns the nodes hosting in-cluster SSH bastion pods.
// A node whose kubelet is stopped marks its pods NotReady, which removes the
// bastion from its Service and breaks SSH to every node, including the
// recovery SSH that restarts kubelet. Returns no nodes when no bastion is
// deployed and an error when the pods cannot be listed, so a failed lookup is
// not mistaken for a cluster without a bastion.
func SSHBastionNodes(ctx context.Context, k8sClient client.Client) ([]string, error) {
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.InNamespace(sshBastionNamespace),
		client.MatchingLabels{sshBastionPodLabel: sshBastionPodValue}); err != nil {
		return nil, fmt.Errorf("failed to list SSH bastion pods: %w", err)
	}

	var nodes []string

	for i := range pods.Items {
		if nodeName := pods.Items[i].Spec.NodeName; nodeName != "" {
			nodes = append(nodes, nodeName)
		}
	}

	return nodes, nil
}

// SelectWorkerNode returns a random Ready, schedulable worker node that is not
// in the excludeNodes list. Nodes hosting the in-cluster SSH bastion are also
// avoided when another eligible worker exists.
func SelectWorkerNode(ctx context.Context, k8sClient client.Client, excludeNodes ...string) (*corev1.Node, error) {
	nodeList := &corev1.NodeList{}

	if err := k8sClient.List(ctx, nodeList, client.MatchingLabels{commonlabels.WorkerRole: ""}); err != nil {
		return nil, fmt.Errorf("failed to list worker nodes: %w", err)
	}

	bastionNodes, err := SSHBastionNodes(ctx, k8sClient)
	if err != nil {
		return nil, err
	}

	node := pickRandomReadyNode(nodeList.Items, append(append([]string{}, excludeNodes...), bastionNodes...)...)
	if node == nil {
		node = pickRandomReadyNode(nodeList.Items, excludeNodes...)
	}

	if node == nil {
		return nil, fmt.Errorf("no eligible Ready worker node found (excluded: %v)", excludeNodes)
	}

	return node, nil
}

// controlPlaneRoleLabels are the node-role labels that identify a control-plane
// node. Clusters may carry either or both depending on OCP version; the legacy
// label string is retained here because it is a Kubernetes API value, not a name
// this repo chooses.
var controlPlaneRoleLabels = []string{
	"node-role.kubernetes.io/control-plane",
	"node-role.kubernetes.io/master",
}

// isControlPlaneNode reports whether the node carries a control-plane role label.
func isControlPlaneNode(node *corev1.Node) bool {
	for _, label := range controlPlaneRoleLabels {
		if _, ok := node.Labels[label]; ok {
			return true
		}
	}

	return false
}

// CountControlPlaneNodes returns the total number of nodes carrying a
// control-plane role label. It intentionally counts ALL such nodes
// regardless of Ready/schedulable state, because it is used as a topology guard
// (a real etcd quorum needs at least 3 control-plane nodes) rather than to
// assess current availability.
func CountControlPlaneNodes(ctx context.Context, k8sClient client.Client) (int, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList); err != nil {
		return 0, fmt.Errorf("failed to list nodes: %w", err)
	}

	count := 0

	for i := range nodeList.Items {
		if isControlPlaneNode(&nodeList.Items[i]) {
			count++
		}
	}

	return count, nil
}

// SelectControlPlaneNode returns a random Ready, schedulable control-plane
// node that is not in the excludeNodes list.
func SelectControlPlaneNode(
	ctx context.Context, k8sClient client.Client, excludeNodes ...string,
) (*corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var controlPlaneNodes []corev1.Node

	for i := range nodeList.Items {
		if isControlPlaneNode(&nodeList.Items[i]) {
			controlPlaneNodes = append(controlPlaneNodes, nodeList.Items[i])
		}
	}

	node := pickRandomReadyNode(controlPlaneNodes, excludeNodes...)
	if node == nil {
		return nil, fmt.Errorf("no eligible Ready control-plane node found (excluded: %v)", excludeNodes)
	}

	return node, nil
}

// CountReadyWorkerNodes returns the number of Ready, schedulable worker nodes.
func CountReadyWorkerNodes(ctx context.Context, k8sClient client.Client) (int, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList, client.MatchingLabels{"node-role.kubernetes.io/worker": ""}); err != nil {
		return 0, fmt.Errorf("failed to list worker nodes: %w", err)
	}

	count := 0

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		if node.Spec.Unschedulable {
			continue
		}

		if IsNodeReady(node) {
			count++
		}
	}

	return count, nil
}

// ListSchedulableWorkerNodes returns Ready, schedulable nodes that carry the worker role and
// do NOT carry a control-plane role label. Excluding control-plane nodes keeps
// resilience/cordon tests from touching control-plane capacity on compact clusters.
func ListSchedulableWorkerNodes(ctx context.Context, k8sClient client.Client) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList, client.MatchingLabels{"node-role.kubernetes.io/worker": ""}); err != nil {
		return nil, fmt.Errorf("failed to list worker nodes: %w", err)
	}

	var eligible []corev1.Node

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		if node.Spec.Unschedulable || !IsNodeReady(node) {
			continue
		}

		if _, hasMaster := node.Labels["node-role.kubernetes.io/master"]; hasMaster {
			continue
		}

		if _, hasCP := node.Labels["node-role.kubernetes.io/control-plane"]; hasCP {
			continue
		}

		eligible = append(eligible, *node)
	}

	return eligible, nil
}

// LogNodeState prints the scheduling and health state of each named node:
// Ready condition with its last heartbeat, cordon flag, taints, and boot ID.
// It is intended for failure diagnostics, so lookup errors are logged rather
// than returned.
func LogNodeState(ctx context.Context, k8sClient client.Client,
	logf func(string, ...interface{}), nodeNames ...string,
) {
	for _, nodeName := range nodeNames {
		if nodeName == "" {
			continue
		}

		node := &corev1.Node{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			logf("Node %s: get failed: %v\n", nodeName, err)

			continue
		}

		ready := "Unknown"
		heartbeat := "never"

		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready = string(cond.Status)
				heartbeat = cond.LastHeartbeatTime.UTC().Format("15:04:05")
			}
		}

		taints := make([]string, 0, len(node.Spec.Taints))
		for _, taint := range node.Spec.Taints {
			taints = append(taints, fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect))
		}

		logf("Node %s: Ready=%s lastHeartbeat=%s unschedulable=%t bootID=%s taints=%v\n",
			nodeName, ready, heartbeat, node.Spec.Unschedulable, node.Status.NodeInfo.BootID, taints)
	}
}
