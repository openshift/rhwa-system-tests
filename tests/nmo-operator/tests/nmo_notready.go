package tests

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoutils"
)

// OCP-71186 ports the Python test_nmo_for_not_ready_node: maintenance can be started on
// a NotReady node. Kubelet is stopped on a control-plane node that does not host the NMO
// controller pod (so the controller is never the component under disruption), the node
// becomes NotReady, and a NodeMaintenance CR is created for it. The drain cannot evict
// pods from a NotReady node, so the CR is expected to reach Running -- not Succeeded --
// while the node carries the exclude-from-remediation label. Deleting the CR removes the
// maintenance taints, and kubelet is restarted via SSH: oc debug cannot recover a node
// whose kubelet is stopped, so the scenario depends on SSH end to end, matching the
// Python reference (invoke_ssh_on_the_node).
var _ = Describe("NMO Maintenance on a NotReady Node",
	Serial,
	Ordered,
	Label(
		labels.OperatorNMO,
		labels.DisruptionDestructive,
		labels.TierAcceptance,
		labels.PlatformAny,
		labels.FrequencyWeekly,
		labels.ComponentController,
	),
	func() {
		var (
			targetNodeName string
			nmCRName       string
		)

		BeforeAll(func() {
			By("Checking SSH access is available for kubelet stop/start")

			if !isSSHAvailable() {
				Skip("SSH not available -- starting maintenance on a NotReady node requires SSH to restart kubelet")
			}

			By("Registering NMO API scheme")
			err := APIClient.AttachScheme(nmov1beta1.AddToScheme)
			Expect(err).ToNot(HaveOccurred(), "Failed to register NMO scheme")

			By("Verifying NMO deployment is Ready")
			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NMO deployment")
			Expect(nmoDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NMO deployment is not Ready")

			By("Selecting a control-plane node that hosts neither the NMO controller pod nor the SSH bastion")
			_, nmoPodNodeName := nmoControllerPod()
			bastionNodes, err := helpers.SSHBastionNodes(context.Background(), APIClient)
			Expect(err).ToNot(HaveOccurred(), "Failed to list SSH bastion nodes")
			excludedNodes := append([]string{nmoPodNodeName}, bastionNodes...)
			controlPlaneNode, err := helpers.SelectControlPlaneNode(
				context.Background(), APIClient, excludedNodes...)
			if err != nil && !strings.Contains(err.Error(), "no eligible Ready control-plane node") {
				Expect(err).ToNot(HaveOccurred(), "Failed to select a control-plane node")
			}

			if controlPlaneNode == nil {
				// No eligible candidate is an environment limitation (SNO or a compact
				// cluster whose control-plane nodes all host the NMO pod or the SSH
				// bastion), not a product failure: Skip instead of failing red. The
				// error-message match above keeps every other failure mode red, so a
				// wording change in the helper degrades to a failure, not a silent skip.
				Skip(fmt.Sprintf("no eligible control-plane node excluding %v: %v", excludedNodes, err))
			}
			targetNodeName = controlPlaneNode.Name
			nmCRName = nmoparams.NMNamePrefix + targetNodeName
			By(fmt.Sprintf("Selected control-plane node %s (NMO controller pod runs on %s)",
				targetNodeName, nmoPodNodeName))

			By("Pre-cleaning any stale NodeMaintenance targeting the selected node")
			// Control-plane teardown (finalizer, uncordon) can exceed the worker-scale
			// UncordonTimeout the other callers pass for worker targets.
			deleteNMsForNode(context.Background(), targetNodeName, nmoparams.ControlPlaneMaintenanceTimeout)
		})

		It("Verify maintenance can be started on a NotReady node",
			reportxml.ID("71186"),
			func() {
				ctx := context.Background()

				// Registered before kubelet is stopped: a failure in any later step must
				// still recover the node.
				DeferCleanup(func() {
					recoverNotReadyNode(targetNodeName, nmCRName)
				})

				By(fmt.Sprintf("Stopping kubelet on node %s via SSH", targetNodeName))
				Expect(helpers.StopKubeletSSH(ctx, APIClient, targetNodeName, nmoparams.SSHTimeout)).To(Succeed(),
					"Failed to stop kubelet on node %s", targetNodeName)

				By("Waiting for the node Ready condition to become Unknown")
				Eventually(func() (corev1.ConditionStatus, error) {
					node, err := getTargetNode(ctx, targetNodeName)
					if err != nil {
						return "", err
					}

					return nodeReadyConditionStatus(node)
				}, nmoparams.NodeNotReadyTimeout, nmoparams.DefaultPollInterval).Should(
					Equal(corev1.ConditionUnknown),
					"Node %s Ready condition should become Unknown after kubelet is stopped",
					targetNodeName)

				By(fmt.Sprintf("Creating NodeMaintenance CR %s for the NotReady node", nmCRName))
				Expect(APIClient.Create(ctx, newNodeMaintenanceWithReason(
					nmCRName, targetNodeName, nmoparams.NotReadyReason))).To(Succeed(),
					"Failed to create NodeMaintenance CR for node %s", targetNodeName)

				By("Waiting for the NodeMaintenance to reach Running phase (the drain cannot complete on a NotReady node)")
				waitForMaintenanceRunning(ctx, nmCRName)

				By(fmt.Sprintf("Verifying node %s carries the exclude-from-remediation label", targetNodeName))
				Eventually(func() (string, error) {
					node, err := getTargetNode(ctx, targetNodeName)
					if err != nil {
						return "", err
					}

					return node.Labels[nmoutils.ExcludeRemediationLabel], nil
				}, nmoparams.MaintenanceTimeout, nmoparams.DefaultPollInterval).Should(Equal("true"),
					"Node %s should carry %s=true while under maintenance",
					targetNodeName, nmoutils.ExcludeRemediationLabel)

				By(fmt.Sprintf("Deleting NodeMaintenance CR %s and waiting for removal", nmCRName))
				deleteAndWaitForNMCR(ctx, nmCRName, nmoparams.MaintenanceTimeout)

				By("Verifying maintenance stopped: the maintenance taints and the exclude-from-remediation label are removed")
				Eventually(func() (bool, error) {
					node, err := getTargetNode(ctx, targetNodeName)
					if err != nil {
						return false, err
					}

					return !hasNoScheduleTaint(node.Spec.Taints, nmoparams.DrainTaintKey) &&
						!hasNoScheduleTaint(node.Spec.Taints, nmoparams.UnschedulableTaintKey) &&
						node.Labels[nmoutils.ExcludeRemediationLabel] != "true", nil
				}, nmoparams.MaintenanceTimeout, nmoparams.DefaultPollInterval).Should(BeTrue(),
					"Node %s should lose the %s and %s taints and the %s label after maintenance stops",
					targetNodeName, nmoparams.DrainTaintKey, nmoparams.UnschedulableTaintKey,
					nmoutils.ExcludeRemediationLabel)

				By(fmt.Sprintf("Starting kubelet on node %s via SSH", targetNodeName))
				Expect(helpers.StartKubeletSSH(ctx, APIClient, targetNodeName, nmoparams.SSHTimeout)).To(Succeed(),
					"Failed to start kubelet on node %s", targetNodeName)

				By("Waiting for the node to become Ready again")
				Expect(helpers.WaitForNodeReady(
					ctx, APIClient, targetNodeName,
					nmoparams.DefaultPollInterval, nmoparams.RebootTimeout,
					GinkgoWriter.Printf,
				)).To(Succeed(), "Node %s did not become Ready after kubelet restart", targetNodeName)
			})
	})

// isSSHAvailable reports whether an SSH private key can be found. This spec needs SSH to
// restart kubelet on the NotReady node: oc debug cannot schedule a debug pod while
// kubelet is down, so without SSH the node could not be recovered after the test.
func isSSHAvailable() bool {
	_, err := helpers.FindSSHKey()

	return err == nil
}

// getTargetNode fetches a fresh copy of the target node for polling assertions, so no
// assertion reads a stale snapshot taken before the disruption.
func getTargetNode(ctx context.Context, nodeName string) (*corev1.Node, error) {
	node := &corev1.Node{}
	if err := APIClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		return nil, fmt.Errorf("getting node %s: %w", nodeName, err)
	}

	return node, nil
}

// nodeReadyConditionStatus returns the status of the node's Ready condition. The Python
// reference waits for Unknown specifically (await_node_status_cond_type with
// NODE_READY/NODE_UNKNOWN), which is why this is not a plain !Ready check.
func nodeReadyConditionStatus(node *corev1.Node) (corev1.ConditionStatus, error) {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status, nil
		}
	}

	return "", fmt.Errorf("node %s has no Ready condition", node.Name)
}

// waitForMaintenanceRunning blocks until the named NodeMaintenance reaches the Running
// phase. On a NotReady node the drain cannot complete, so Running -- not Succeeded --
// is the expected phase for this scenario (Python reference: 180s timeout).
func waitForMaintenanceRunning(ctx context.Context, name string) {
	GinkgoHelper()

	Eventually(func() (nmov1beta1.MaintenancePhase, error) {
		current := &nmov1beta1.NodeMaintenance{}
		getErr := APIClient.Get(ctx, client.ObjectKey{Name: name}, current)
		if errors.IsNotFound(getErr) {
			// The CR we just created is gone -- no point retrying, fail immediately.
			return "", StopTrying(fmt.Sprintf(
				"NodeMaintenance %s disappeared while awaiting Running phase", name)).Wrap(getErr)
		}

		if getErr != nil {
			return "", fmt.Errorf("getting NodeMaintenance %s: %w", name, getErr)
		}

		return current.Status.Phase, nil
	}, nmoparams.MaintenanceRunningTimeout, nmoparams.DefaultPollInterval).Should(
		Equal(nmov1beta1.MaintenanceRunning),
		"NodeMaintenance %s did not reach Running phase", name)
}

// recoverNotReadyNode restores the cluster after the NotReady-node scenario. The
// NodeMaintenance CR is deleted first: NMO's stopMaintenance uncordons and untaints
// through the API and needs no running kubelet, and deleting first closes the window in
// which a kubelet restart would let the previously blocked drain complete and evict
// control-plane pods. Kubelet is then restarted only when the node is actually NotReady
// (the Python teardown guard -- a start on a running kubelet is a no-op, but flaky SSH
// against a healthy node would burn the full retry budget). Every step is best-effort:
// a failure logs a warning and the remaining steps still run, so one failing step can
// never leave a control-plane node NotReady for the rest of the suite.
func recoverNotReadyNode(nodeName, nmName string) {
	cleanupCtx := context.Background()

	By(fmt.Sprintf("Cleanup: deleting NodeMaintenance CR %s if it still exists", nmName))
	deleteNMControlPlaneBestEffort(cleanupCtx, nmName)

	By(fmt.Sprintf("Cleanup: starting kubelet on node %s if it is NotReady", nodeName))
	node := &corev1.Node{}
	getErr := APIClient.Get(cleanupCtx, client.ObjectKey{Name: nodeName}, node)
	if getErr != nil || !helpers.IsNodeReady(node) {
		// On a read failure the node state is unknown, so attempt the restart anyway:
		// starting an already-running kubelet is a no-op, a missed restart is not.
		if sshErr := helpers.StartKubeletSSHWithRetry(
			cleanupCtx, APIClient, nodeName, nmoparams.RebootTimeout, GinkgoWriter.Printf,
		); sshErr != nil {
			GinkgoWriter.Printf("WARNING: failed to restart kubelet on %s: %v\n", nodeName, sshErr)
			AddReportEntry("notready-kubelet-restart-failed",
				fmt.Sprintf("node %s: %v", nodeName, sshErr))
		}
	}

	By(fmt.Sprintf("Cleanup: waiting for node %s to become Ready, uncordoned and untainted", nodeName))
	waitForControlPlaneRecovery(cleanupCtx, nodeName)
}

// deleteNMControlPlaneBestEffort deletes the NodeMaintenance CR and waits until it is
// gone, with a control-plane-scale budget. It mirrors deleteNMBestEffort, which
// hardcodes the worker-scale UncordonTimeout that control-plane teardown can plausibly
// exceed -- the quorum spec gives control-plane cleanup ControlPlaneMaintenanceTimeout
// for the same reason. Cleanup-safe: PollUntilContextTimeout with warning logging,
// never Fail.
func deleteNMControlPlaneBestEffort(ctx context.Context, name string) {
	nm := &nmov1beta1.NodeMaintenance{ObjectMeta: metav1.ObjectMeta{Name: name}}

	waitErr := wait.PollUntilContextTimeout(
		ctx, nmoparams.DefaultPollInterval, nmoparams.ControlPlaneMaintenanceTimeout, true,
		func(ctx context.Context) (bool, error) {
			if delErr := APIClient.Delete(ctx, nm); delErr != nil && !errors.IsNotFound(delErr) {
				GinkgoWriter.Printf("WARNING: deleting NodeMaintenance %s failed, retrying: %v\n", name, delErr)

				return false, nil
			}

			getErr := APIClient.Get(ctx, client.ObjectKey{Name: name}, &nmov1beta1.NodeMaintenance{})
			if getErr != nil && !errors.IsNotFound(getErr) {
				GinkgoWriter.Printf("WARNING: checking NodeMaintenance %s after delete failed, retrying: %v\n", name, getErr)

				return false, nil
			}

			return errors.IsNotFound(getErr), nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: NodeMaintenance %s not confirmed deleted within timeout: %v\n", name, waitErr)
	}
}

// waitForControlPlaneRecovery waits until the node is Ready, uncordoned and free of
// both maintenance taints. It mirrors waitForNodeRecoveryBestEffort but with a
// control-plane-scale budget, and also checks the node.kubernetes.io/unschedulable
// taint that the shared helper omits. Cleanup-safe: warning logging, never Fail.
func waitForControlPlaneRecovery(ctx context.Context, nodeName string) {
	waitErr := wait.PollUntilContextTimeout(
		ctx, nmoparams.DefaultPollInterval, nmoparams.ControlPlaneMaintenanceTimeout, true,
		func(ctx context.Context) (bool, error) {
			node := &corev1.Node{}
			if getErr := APIClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); getErr != nil {
				GinkgoWriter.Printf("WARNING: getting node %s during recovery, retrying: %v\n", nodeName, getErr)

				return false, nil
			}

			return helpers.IsNodeReady(node) && !node.Spec.Unschedulable &&
				!hasNoScheduleTaint(node.Spec.Taints, nmoparams.DrainTaintKey) &&
				!hasNoScheduleTaint(node.Spec.Taints, nmoparams.UnschedulableTaintKey), nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: node %s did not fully recover (Ready, uncordoned, untainted) within %s: %v\n",
			nodeName, nmoparams.ControlPlaneMaintenanceTimeout, waitErr)
	}
}
