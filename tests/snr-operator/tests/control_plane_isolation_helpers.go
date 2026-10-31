package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// agentNamespaceScript wraps iptablesCmds in a shell script that finds the SNR
// agent process in the host PID namespace and runs each command inside that
// process's network namespace.
//
// The agent DaemonSet sets hostPID but not hostNetwork, so the agent is visible
// to pgrep from a hostPID pod while its peer traffic stays inside the pod
// network namespace. Rules added with "nsenter --target 1 --net" would land on
// the host and never match that traffic.
func agentNamespaceScript(iptablesCmds []string) string {
	var builder strings.Builder

	builder.WriteString("set -e\n")
	fmt.Fprintf(&builder, "pid=$(pgrep -f %q | head -1)\n", snrparams.AgentProcessPattern)
	builder.WriteString(`if [ -z "$pid" ]; then echo AGENT_PID_NOT_FOUND; exit 1; fi` + "\n")
	builder.WriteString(`echo "agent pid: $pid"` + "\n")

	for _, cmd := range iptablesCmds {
		fmt.Fprintf(&builder, "nsenter -t \"$pid\" -n -- %s\n", cmd)
	}

	builder.WriteString("echo DONE\n")

	return builder.String()
}

// peerPortRuleCmds returns the iptables commands that add (-I) or remove (-D)
// the REJECT rules for the SNR peer health port. Removal tolerates a missing
// rule so the flush path stays idempotent.
func peerPortRuleCmds(operation string, peerPort int) []string {
	suffix := ""
	if operation == "-D" {
		suffix = " || true"
	}

	return []string{
		fmt.Sprintf("iptables %s OUTPUT -p tcp --dport %d -j REJECT%s", operation, peerPort, suffix),
		fmt.Sprintf("iptables %s INPUT -p tcp --dport %d -j REJECT%s", operation, peerPort, suffix),
	}
}

// agentProbeScript proves the agent PID can be found and that iptables works
// inside its network namespace, so a node where either fails skips the test
// instead of failing part-way through.
func agentProbeScript() string {
	return agentNamespaceScript([]string{"iptables -L OUTPUT -n"})
}

// deleteHostCommandPod removes a named pod and waits for it to be gone,
// tolerating a pod that never existed.
func deleteHostCommandPod(podName string) {
	builder, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
	if pullErr != nil || builder == nil {
		return
	}

	if _, delErr := builder.DeleteAndWait(snrparams.CPIsolationPodTimeout); delErr != nil {
		GinkgoWriter.Printf("Warning: could not delete pod %s: %v\n", podName, delErr)
	}
}

// runHostCommandPod runs script to completion in a privileged hostPID pod on
// nodeName and returns its final phase and log. The pod is removed before it
// returns, so a killed run does not block the next one.
func runHostCommandPod(nodeName, podName, script string) (corev1.PodPhase, string) {
	deleteHostCommandPod(podName)
	_, createErr := pod.NewBuilder(
		APIClient, podName, medik8sparams.OperatorNs, snrparams.InjectorImage).
		DefineOnNode(nodeName).
		WithHostPid(true).
		WithPrivilegedFlag().
		WithRestartPolicy(corev1.RestartPolicyNever).
		RedefineDefaultCMD([]string{"sh", "-c", script}).
		Create()
	Expect(createErr).ToNot(HaveOccurred(),
		"Host command pod %q must be created on node %q", podName, nodeName)

	defer deleteHostCommandPod(podName)

	var (
		phase  corev1.PodPhase
		podLog string
	)

	Eventually(func() bool {
		current, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs)
		if pullErr != nil || current == nil {
			return false
		}
		phase = current.Object.Status.Phase

		return phase == corev1.PodSucceeded || phase == corev1.PodFailed
	}, snrparams.CPIsolationPodTimeout, snrparams.DefaultPollInterval).Should(BeTrue(),
		"Host command pod %q on node %q must reach a terminal phase", podName, nodeName)

	if current, pullErr := pod.Pull(APIClient, podName, medik8sparams.OperatorNs); pullErr == nil && current != nil {
		if logText, logErr := current.GetLog(snrparams.CPIsolationPodTimeout, ""); logErr == nil {
			podLog = logText
		}
	}

	GinkgoWriter.Printf("Host command pod %s on %s finished %s:\n%s\n", podName, nodeName, phase, podLog)

	return phase, podLog
}

// agentNamespaceUsable reports whether the SNR agent process can be found on
// nodeName and iptables runs inside its network namespace.
func agentNamespaceUsable(nodeName string) (bool, string) {
	phase, podLog := runHostCommandPod(nodeName, snrparams.CPIsolationProbePodName, agentProbeScript())

	return phase == corev1.PodSucceeded, podLog
}

// blockAgentPeerPort rejects traffic on the SNR peer health port inside the
// agent's network namespace on nodeName, isolating it from every peer at once
// while leaving its kube-apiserver connection untouched.
func blockAgentPeerPort(nodeName string, peerPort int) {
	phase, podLog := runHostCommandPod(nodeName, snrparams.CPIsolationInjectorPodName,
		agentNamespaceScript(peerPortRuleCmds("-I", peerPort)))
	Expect(phase).To(Equal(corev1.PodSucceeded),
		"Injector pod must block port %d in the agent namespace on node %q: %s", peerPort, nodeName, podLog)
}

// flushAgentPeerPort removes the REJECT rules again. It never fails the suite:
// after the victim reboots the agent has a fresh network namespace and there is
// nothing left to flush.
func flushAgentPeerPort(nodeName string, peerPort int) {
	phase, podLog := runHostCommandPod(nodeName, snrparams.CPIsolationFlusherPodName,
		agentNamespaceScript(peerPortRuleCmds("-D", peerPort)))
	if phase != corev1.PodSucceeded {
		GinkgoWriter.Printf("Warning: peer port flush on node %s ended %s (expected after a reboot): %s\n",
			nodeName, phase, podLog)
	}
}

// peerHealthPortFromSNRC reads spec.hostPort from the default
// SelfNodeRemediationConfig, falling back to the CRD default when it is unset.
func peerHealthPortFromSNRC(ctx context.Context) int {
	snrc := buildSNRCR("SelfNodeRemediationConfig", snrparams.SNRConfigName, nil)
	if err := APIClient.Get(ctx,
		client.ObjectKey{Name: snrparams.SNRConfigName, Namespace: medik8sparams.OperatorNs},
		snrc); err != nil {
		GinkgoWriter.Printf("Could not read SNRC hostPort, using default %d: %v\n",
			snrparams.DefaultPeerHostPort, err)

		return snrparams.DefaultPeerHostPort
	}

	hostPort, found, err := unstructured.NestedInt64(snrc.Object, "spec", "hostPort")
	if err != nil || !found || hostPort == 0 {
		return snrparams.DefaultPeerHostPort
	}

	return int(hostPort)
}

// agentPodOnNode returns the SNR agent DaemonSet pod running on nodeName.
func agentPodOnNode(nodeName string) (*pod.Builder, error) {
	dsPods, err := pod.List(APIClient, medik8sparams.OperatorNs,
		metav1.ListOptions{LabelSelector: snrparams.DaemonSetPodLabelSelector})
	if err != nil {
		return nil, err
	}

	for _, dsPod := range dsPods {
		if dsPod.Object.Spec.NodeName == nodeName {
			return dsPod, nil
		}
	}

	return nil, fmt.Errorf("no SNR agent pod found on node %q", nodeName)
}

// agentLogOnNode returns the recent log of the agent pod on nodeName and whether
// it could be read. Scoped to one node so a message emitted by a different node's
// agent cannot be observed. Callers that need several messages should read the
// log once with this and assert against the returned snapshot, rather than
// re-reading per message: once the victim reboots, its agent pod is replaced and
// the new pod's log no longer holds the pre-reboot messages.
func agentLogOnNode(nodeName string, logWindow time.Duration) (string, bool) {
	agentPod, err := agentPodOnNode(nodeName)
	if err != nil {
		GinkgoWriter.Printf("Could not find agent pod on node %s: %v\n", nodeName, err)

		return "", false
	}

	logText, err := agentPod.GetLog(logWindow, snrparams.ManagerContainerName)
	if err != nil {
		GinkgoWriter.Printf("Could not read agent log on node %s: %v\n", nodeName, err)

		return "", false
	}

	return logText, true
}

// nhcSelectsControlPlane reports whether any NodeHealthCheck in the cluster
// selects control plane nodes. One would remediate the victim through the
// ordinary NHC path, so the test skips rather than reporting a false pass.
//
// It returns an error when the NHC inventory cannot be determined. When the
// NodeHealthCheck CRD is not installed the cluster has no NHC, so it returns
// (false, nil); any other list error is returned so the precondition fails
// instead of silently proceeding as if no NHC selected the victim.
func nhcSelectsControlPlane(ctx context.Context) (bool, error) {
	nhcList := medik8sparams.NewUnstructuredList(
		snrparams.NHCAPIGroup, snrparams.NHCAPIVersion, "NodeHealthCheck")
	if err := APIClient.List(ctx, nhcList); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("could not list NodeHealthCheck CRs: %w", err)
	}

	for idx := range nhcList.Items {
		if nhcSpecSelectsControlPlane(nhcList.Items[idx].Object) {
			GinkgoWriter.Printf("NodeHealthCheck %q selects control plane nodes\n", nhcList.Items[idx].GetName())

			return true, nil
		}
	}

	return false, nil
}

// nhcSpecSelectsControlPlane parses a NodeHealthCheck's spec.selector (a standard
// metav1.LabelSelector) and reports whether it positively selects control plane
// nodes. It inspects matchLabels and matchExpressions for a control plane role
// key, counting an expression only when its operator is In or Exists. This avoids
// the false positive a plain substring scan hit on a worker-only NHC that
// excludes control plane nodes with a DoesNotExist expression on the same key.
//
// This is a conservative check on the selector shape, not a full label match
// against the victim node (the victim is not yet chosen when the precondition
// runs). A NHC whose selector matches control plane nodes through unrelated
// custom labels would not be detected here.
func nhcSpecSelectsControlPlane(obj map[string]interface{}) bool {
	selector, found, err := unstructured.NestedMap(obj, "spec", "selector")
	if err != nil || !found {
		return false
	}

	matchLabels, _, _ := unstructured.NestedStringMap(selector, "matchLabels")
	for key := range matchLabels {
		if isControlPlaneRoleKey(key) {
			return true
		}
	}

	expressions, _, _ := unstructured.NestedSlice(selector, "matchExpressions")
	for _, raw := range expressions {
		expr, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		key, _, _ := unstructured.NestedString(expr, "key")
		operator, _, _ := unstructured.NestedString(expr, "operator")

		if isControlPlaneRoleKey(key) &&
			(operator == string(metav1.LabelSelectorOpIn) ||
				operator == string(metav1.LabelSelectorOpExists)) {
			return true
		}
	}

	return false
}

// isControlPlaneRoleKey reports whether key is one of the standard node role
// labels that mark a control plane node.
func isControlPlaneRoleKey(key string) bool {
	return key == "node-role.kubernetes.io/control-plane" ||
		key == "node-role.kubernetes.io/master"
}
