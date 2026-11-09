package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/nodes"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// The NMO pod eviction test (OCP-29600) ports the Python test_eviction_of_nmo_pod: the
// node hosting the NMO controller pod is put into maintenance, and the controller pod
// must be evicted and come back running on a different node. The controller tolerates
// the control-plane taints and prefers those nodes when no infra nodes exist, so the
// maintenance target is often a control-plane node; the etcd quorum budget is guarded
// on both sides of the maintenance because of that.
var _ = Describe("NMO Pod Eviction",
	Serial,
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorNMO, nmoparams.Label,
		labels.TierAcceptance, labels.FrequencyWeekly),
	func() {
		var ctx context.Context

		BeforeAll(func() {
			ctx = context.Background()

			By("Registering NMO API scheme")
			Expect(APIClient.AttachScheme(nmov1beta1.AddToScheme)).To(Succeed(),
				"Failed to register NMO scheme")

			By("Verifying NMO operator deployment is ready")

			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NMO deployment")
			Expect(nmoDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NMO deployment is not Ready")
		})

		It("evicts the NMO pod when its node enters maintenance",
			reportxml.ID("29600"),
			Label(
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.ComponentController,
			),
			func() {
				By("Identifying the NMO controller pod and its node")

				nmoPodName, nmoPodNode := nmoControllerPod()
				GinkgoWriter.Printf("NMO controller pod %s runs on node %s\n", nmoPodName, nmoPodNode)
				nmCRName := nmoparams.EvictionNMNamePrefix + nmoPodNode

				By(fmt.Sprintf("Pre-cleaning stale NodeMaintenance CRs for node %s", nmoPodNode))

				deleteNMsForNode(ctx, nmoPodNode, nmoparams.UncordonTimeout)

				By(fmt.Sprintf("Checking whether node %s is a control-plane node", nmoPodNode))

				targetNode, pullErr := nodes.Pull(APIClient, nmoPodNode)
				Expect(pullErr).ToNot(HaveOccurred(), "Failed to get node %s", nmoPodNode)
				targetIsControlPlane := isControlPlane(targetNode.Object)
				nmDeleteBudget, nodeRecoveryBudget := evictionCleanupBudgets(targetIsControlPlane)

				if targetIsControlPlane {
					By(fmt.Sprintf(
						"Node %s is control-plane; waiting until the etcd quorum budget allows a disruption",
						nmoPodNode))
					waitForEtcdQuorumAvailable(ctx)
				}

				By("Registering cleanup to remove the maintenance and recover the node")

				DeferCleanup(func() {
					cleanupCtx := context.Background()

					// The NodeMaintenance finalizer is removed by the controller, and
					// this spec evicts the controller mid-drain: wait for its
					// replacement first, otherwise the deletion below can stall for
					// its whole budget with a CR that cannot disappear.
					By("Waiting for the NMO deployment to be Ready again")
					waitForNMODeploymentReadyBestEffort(cleanupCtx)

					By(fmt.Sprintf("Cleaning up NodeMaintenance %s", nmCRName))
					deleteNMBestEffortWithBudget(cleanupCtx, nmCRName, nmDeleteBudget)

					By(fmt.Sprintf("Waiting for node %s to become Ready and uncordoned", nmoPodNode))
					waitForNodeRecoveryBestEffortWithBudget(cleanupCtx, nmoPodNode, nodeRecoveryBudget)

					By("Waiting for the etcd quorum budget to allow disruptions again")
					waitForEtcdQuorumRestoredBestEffort(cleanupCtx)
				})

				By(fmt.Sprintf("Creating NodeMaintenance %s for node %s", nmCRName, nmoPodNode))

				nmCR := newNodeMaintenanceWithReason(nmCRName, nmoPodNode, nmoparams.EvictionReason)
				Expect(APIClient.Create(ctx, nmCR)).To(Succeed(),
					"Failed to create NodeMaintenance %s for node %s", nmCRName, nmoPodNode)
				waitForEvictionMaintenanceSucceeded(ctx, nmCRName)

				By("Verifying the node is cordoned and carries both NoSchedule taints")

				assertNodeCordonAndTaint(nmoPodNode, true, nmoparams.ControlPlaneMaintenanceTimeout)
				assertNodeUnschedulableTaint(nmoPodNode)

				By("Verifying the NodeMaintenance CR reports the drain completed")

				assertDrainCompleted(ctx, nmCRName)

				By(fmt.Sprintf(
					"Verifying only DaemonSet or Node-owned pods remain on node %s", nmoPodNode))

				assertRemainingPodsAreDaemonSetOrNodeOwned(ctx, nmoPodNode)

				By("Verifying the NMO controller pod runs on a different node")

				waitForNMOControllerOffNode(nmoPodNode)
			})
	})

// waitForEvictionMaintenanceSucceeded waits until the NodeMaintenance CR reaches the
// Succeeded phase. It does not reuse waitForMaintenanceSucceeded because that helper
// budgets the 5-minute MaintenanceTimeout, while the eviction target is often a
// control-plane node and the drain evicts the operator itself, which then resumes the
// reconcile from its replacement pod -- the control-plane budget covers both.
func waitForEvictionMaintenanceSucceeded(ctx context.Context, name string) {
	GinkgoHelper()

	By("Waiting for the NodeMaintenance to reach Succeeded phase")

	Eventually(func() (nmov1beta1.MaintenancePhase, error) {
		current := &nmov1beta1.NodeMaintenance{}
		getErr := APIClient.Get(ctx, client.ObjectKey{Name: name}, current)
		if errors.IsNotFound(getErr) {
			// The CR we just created is gone -- no point retrying, fail immediately.
			return "", StopTrying(fmt.Sprintf(
				"NodeMaintenance %s disappeared while awaiting Succeeded phase", name)).Wrap(getErr)
		}

		if getErr != nil {
			// Transient API errors keep polling until the timeout and surface in the
			// failure message; only a CR that disappeared fails fast, via StopTrying.
			return "", getErr
		}

		return current.Status.Phase, nil
	}, nmoparams.ControlPlaneMaintenanceTimeout, nmoparams.DefaultPollInterval).Should(
		Equal(nmov1beta1.MaintenanceSucceeded),
		"NodeMaintenance %s did not reach Succeeded phase", name)
}

// assertNodeUnschedulableTaint verifies the node carries the
// node.kubernetes.io/unschedulable NoSchedule taint, which the node controller adds to
// a cordoned node. assertNodeCordonAndTaint covers the medik8s.io/drain taint and the
// cordon flag itself.
func assertNodeUnschedulableTaint(nodeName string) {
	GinkgoHelper()

	Eventually(func(assertion Gomega) {
		node, err := nodes.Pull(APIClient, nodeName)
		assertion.Expect(err).ToNot(HaveOccurred(), "Failed to get node %s", nodeName)
		assertion.Expect(hasNoScheduleTaint(node.Object.Spec.Taints, nmoparams.UnschedulableTaintKey)).To(BeTrue(),
			"Node %s should carry the %s NoSchedule taint while under maintenance",
			nodeName, nmoparams.UnschedulableTaintKey)
	}, nmoparams.ControlPlaneMaintenanceTimeout, nmoparams.DefaultPollInterval).Should(Succeed())
}

// assertRemainingPodsAreDaemonSetOrNodeOwned mirrors the Python are_all_pods_evicted:
// once the drain has succeeded, every pod left on the maintained node must be owned by
// a DaemonSet or by the Node itself (static and mirror pods). Unlike the Python, which
// inspected Running+Ready pods only, finished and terminating pods are ignored here
// explicitly, and so are the ownerless control-plane guard pods, which tolerate every
// taint and are recreated on the node by design; the Python's SNR wait is not ported
// because SNR pods are DaemonSet-owned and pass the ownership check anyway.
func assertRemainingPodsAreDaemonSetOrNodeOwned(ctx context.Context, nodeName string) {
	GinkgoHelper()
	podList := &corev1.PodList{}
	Expect(APIClient.List(ctx, podList)).To(Succeed(),
		"Failed to list pods while checking the remaining pods on node %s", nodeName)

	var offenders []string

	for podIdx := range podList.Items {
		podItem := &podList.Items[podIdx]
		if podItem.Spec.NodeName != nodeName || isIgnoredAfterDrain(podItem) {
			continue
		}

		if !hasDaemonSetOrNodeOwner(podItem.OwnerReferences) {
			offenders = append(offenders, fmt.Sprintf("%s/%s", podItem.Namespace, podItem.Name))
		}
	}

	Expect(offenders).To(BeEmpty(),
		"Pods remaining on node %s after the drain are not DaemonSet or Node-owned: %s",
		nodeName, strings.Join(offenders, ", "))
}

// hasDaemonSetOrNodeOwner reports whether any owner reference names a DaemonSet or the
// Node itself. Static and mirror pods carry a Node owner reference.
func hasDaemonSetOrNodeOwner(ownerRefs []metav1.OwnerReference) bool {
	for refIdx := range ownerRefs {
		if ownerRefs[refIdx].Kind == "DaemonSet" || ownerRefs[refIdx].Kind == "Node" {
			return true
		}
	}

	return false
}

// isIgnoredAfterDrain reports whether a pod found on the drained node must not count
// as a leftover workload: finished pods (the drainer ignores them, and the Python
// scanned Running+Ready pods only), pods already terminating, and the ownerless guard
// pods the control-plane operators pin to the node.
func isIgnoredAfterDrain(podItem *corev1.Pod) bool {
	if podItem.Status.Phase == corev1.PodSucceeded || podItem.Status.Phase == corev1.PodFailed {
		return true
	}

	if podItem.DeletionTimestamp != nil {
		return true
	}

	return isGuardPod(podItem)
}

// isGuardPod matches the per-node guard pods OpenShift runs without owner references
// (etcd-guard and the kube-apiserver, kube-controller-manager and kube-scheduler
// guards). They tolerate every taint, are force-deleted by the drain, and are
// recreated on the same node by design, so they are not evictable workloads.
func isGuardPod(podItem *corev1.Pod) bool {
	switch podItem.Namespace {
	case nmoparams.EtcdNamespace,
		"openshift-kube-apiserver",
		"openshift-kube-controller-manager",
		"openshift-kube-scheduler":
		return strings.Contains(podItem.Name, "-guard-")
	}

	return false
}

// waitForNMOControllerOffNode waits until a Running, containers-ready NMO controller
// pod exists and none of the running controller pods sits on the evicted node. The
// Python original compared node names once; requiring a ready replacement on a
// different node also proves that the new pod actually came up, and not merely that
// the old pod object disappeared.
func waitForNMOControllerOffNode(evictedNode string) {
	GinkgoHelper()

	Eventually(func(assertion Gomega) {
		allPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: nmoparams.OperatorControllerPodLabelSelector})
		assertion.Expect(listErr).ToNot(HaveOccurred(), "Failed to list NMO controller pods")
		running := helpers.FilterRunningPods(
			helpers.FilterPodsByDeployment(allPods, nmoparams.OperatorDeploymentName))
		assertion.Expect(running).ToNot(BeEmpty(),
			"No running NMO controller pod found in %s", medik8sparams.OperatorNs)

		for podIdx := range running {
			assertion.Expect(running[podIdx].Object.Spec.NodeName).ToNot(Equal(evictedNode),
				"NMO controller pod %s still runs on the evicted node %s",
				running[podIdx].Object.Name, evictedNode)
		}
	}, nmoparams.NMOControllerRescheduleTimeout, nmoparams.DefaultPollInterval).Should(Succeed(),
		"NMO controller pod should be Running and ready on a node other than %s", evictedNode)
}

// etcdQuorumAllowsDisruption reads the openshift-etcd quorum PDB and reports whether
// it currently allows at least one disruption.
func etcdQuorumAllowsDisruption(ctx context.Context) (bool, error) {
	pdb, pdbErr := getEtcdQuorumPDB(ctx)
	if pdbErr != nil {
		return false, pdbErr
	}

	return pdb.Status.DisruptionsAllowed >= nmoparams.ExpectedQuorumDisruptions, nil
}

// waitForEtcdQuorumAvailable blocks until the openshift-etcd quorum PDB allows at
// least one disruption. The admission webhook denies a control-plane NodeMaintenance
// unless it can prove the disruption is allowed, so a maintenance from an earlier
// spec that is still releasing the budget would fail this spec's create instead of
// being waited out. Read errors keep the wait polling until the timeout.
func waitForEtcdQuorumAvailable(ctx context.Context) {
	GinkgoHelper()

	Eventually(func() (bool, error) {
		return etcdQuorumAllowsDisruption(ctx)
	}, nmoparams.QuorumUpdateTimeout, nmoparams.DefaultPollInterval).Should(BeTrue(),
		"etcd quorum PDB does not allow a control-plane disruption; "+
			"a leftover maintenance from an earlier spec may still be cleaning up")
}

// waitForEtcdQuorumRestoredBestEffort waits until the openshift-etcd quorum PDB allows
// a disruption again. The control-plane quorum spec (OCP-46790) reads the budget once
// at startup and skips when it is exhausted, so leaving it at zero here would silently
// skip that spec whenever it runs after this one. Read errors are retried until the
// timeout rather than treated as success. Best-effort: warns on timeout.
func waitForEtcdQuorumRestoredBestEffort(ctx context.Context) {
	waitErr := wait.PollUntilContextTimeout(ctx, nmoparams.DefaultPollInterval,
		nmoparams.QuorumUpdateTimeout, true,
		func(ctx context.Context) (bool, error) {
			allowed, currentErr := etcdQuorumAllowsDisruption(ctx)
			if currentErr != nil {
				GinkgoWriter.Printf(
					"WARNING: reading etcd quorum PDB during cleanup, retrying: %v\n", currentErr)

				return false, nil
			}

			return allowed, nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: etcd quorum PDB did not allow disruptions again within %s: %v\n",
			nmoparams.QuorumUpdateTimeout, waitErr)
	}
}

// evictionCleanupBudgets returns the NodeMaintenance deletion and node recovery
// budgets for the eviction target. Control-plane teardown (finalizer removal,
// uncordon, taint cleanup, guard-pod reschedule) can exceed the worker-scale
// UncordonTimeout, so control-plane targets get the longer budgets, like the quorum
// spec budgets its control-plane teardown.
func evictionCleanupBudgets(targetIsControlPlane bool) (time.Duration, time.Duration) {
	if targetIsControlPlane {
		return nmoparams.ControlPlaneMaintenanceTimeout, nmoparams.RebootTimeout
	}

	return nmoparams.UncordonTimeout, nmoparams.UncordonTimeout
}

// deleteNMBestEffortWithBudget deletes the named NodeMaintenance and waits until it
// is gone, within the given budget. It mirrors deleteNMBestEffort but takes an
// explicit timeout, because the eviction target is often a control-plane node whose
// teardown can exceed the worker-scale UncordonTimeout the shared helper hardcodes.
// Best-effort: warns on timeout.
func deleteNMBestEffortWithBudget(ctx context.Context, name string, timeout time.Duration) {
	nm := &nmov1beta1.NodeMaintenance{ObjectMeta: metav1.ObjectMeta{Name: name}}

	waitErr := wait.PollUntilContextTimeout(ctx, nmoparams.DefaultPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if delErr := APIClient.Delete(ctx, nm); delErr != nil && !errors.IsNotFound(delErr) {
				GinkgoWriter.Printf("WARNING: deleting NodeMaintenance %s failed, retrying: %v\n", name, delErr)

				return false, nil
			}

			getErr := APIClient.Get(ctx, client.ObjectKey{Name: name}, &nmov1beta1.NodeMaintenance{})
			if getErr != nil && !errors.IsNotFound(getErr) {
				GinkgoWriter.Printf("WARNING: checking NodeMaintenance %s after delete failed, retrying: %v\n",
					name, getErr)

				return false, nil
			}

			return errors.IsNotFound(getErr), nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: NodeMaintenance %s not confirmed deleted within %s: %v\n",
			name, timeout, waitErr)
	}
}

// waitForNodeRecoveryBestEffortWithBudget waits until the node is Ready, uncordoned
// and free of the drain taint, within the given budget. It mirrors
// waitForNodeRecoveryBestEffort but takes an explicit timeout, because a control-plane
// node's recovery can exceed the worker-scale UncordonTimeout the shared helper
// hardcodes. Best-effort: warns on timeout.
func waitForNodeRecoveryBestEffortWithBudget(ctx context.Context, nodeName string, timeout time.Duration) {
	waitErr := wait.PollUntilContextTimeout(ctx, nmoparams.DefaultPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			node := &corev1.Node{}
			if err := APIClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
				GinkgoWriter.Printf("WARNING: getting node %s during recovery, retrying: %v\n", nodeName, err)

				return false, nil
			}

			if node.Spec.Unschedulable || hasDrainTaint(node.Spec.Taints) {
				return false, nil
			}

			return helpers.IsNodeReady(node), nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: node %s did not fully recover (Ready, uncordoned, untainted) within %s: %v\n",
			nodeName, timeout, waitErr)
	}
}

// waitForNMODeploymentReadyBestEffort waits until the NMO deployment reports ready
// again, so that the cleanup steps relying on the controller (the NodeMaintenance
// deletion needs it to remove its finalizer) and the specs running after this one
// start with a healthy operator. Transient read errors are retried until the timeout.
// Best-effort: warns when the deployment does not become ready in time.
func waitForNMODeploymentReadyBestEffort(ctx context.Context) {
	waitErr := wait.PollUntilContextTimeout(ctx, nmoparams.DefaultPollInterval,
		nmoparams.NMOControllerRescheduleTimeout, true,
		func(ctx context.Context) (bool, error) {
			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			if err != nil {
				GinkgoWriter.Printf("WARNING: getting NMO deployment during cleanup, retrying: %v\n", err)

				return false, nil
			}

			return nmoDeployment.Object.Status.ReadyReplicas >= nmoparams.ExpectedReplicas, nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: NMO deployment did not become Ready again within %s: %v\n",
			nmoparams.NMOControllerRescheduleTimeout, waitErr)
	}
}
