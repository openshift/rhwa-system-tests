package tests

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// isControlPlane reports whether the node carries a master or control-plane role label.
// On compact topologies control-plane nodes also carry the worker label, so they must be
// filtered out: putting one under maintenance can trip the etcd-quorum webhook and reject
// the first (expected-to-succeed) NodeMaintenance for reasons unrelated to these tests.
func isControlPlane(node *corev1.Node) bool {
	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		return true
	}

	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}

	return false
}

// listSchedulableWorkers returns Ready, schedulable, non-control-plane worker nodes. This is
// the Go equivalent of the Python get_schedulable_nodes_list(node_role=WORKER_ROLE) helper.
func listSchedulableWorkers(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := APIClient.List(ctx, nodeList,
		client.MatchingLabels{"node-role.kubernetes.io/worker": ""}); err != nil {
		return nil, fmt.Errorf("listing worker nodes: %w", err)
	}

	var eligible []corev1.Node

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		if node.Spec.Unschedulable || isControlPlane(node) {
			continue
		}

		if helpers.IsNodeReady(node) {
			eligible = append(eligible, *node)
		}
	}

	return eligible, nil
}

// selectSchedulableWorker returns the name of a random Ready, schedulable, non-control-plane
// worker node (Python: choice(get_schedulable_nodes_list(WORKER_ROLE))). It retries until an
// eligible node appears, so a node still uncordoning from a prior interrupted run does not
// cause a one-shot failure.
func selectSchedulableWorker(ctx context.Context) string {
	GinkgoHelper()

	return selectSchedulableWorkerExcluding(ctx)
}

// selectSchedulableWorkerExcluding is selectSchedulableWorker with the named nodes removed
// from the candidate set. A caller that is about to drain a node passes the node hosting the
// NMO controller pod: draining it would evict the controller, which then reschedules under a
// new name and invalidates any assertion keyed on the pod name. FAR does the same thing for
// its leader node (helpers.SelectWorkerNode with the leader excluded).
func selectSchedulableWorkerExcluding(ctx context.Context, excludeNodes ...string) string {
	GinkgoHelper()

	var selected string

	Eventually(func() (int, error) {
		eligible, err := listSchedulableWorkers(ctx)
		if err != nil {
			return 0, err
		}

		candidates := make([]corev1.Node, 0, len(eligible))

		for _, node := range eligible {
			if !slices.Contains(excludeNodes, node.Name) {
				candidates = append(candidates, node)
			}
		}

		if len(candidates) == 0 {
			return 0, nil
		}

		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
		selected = candidates[rand.Intn(len(candidates))].Name

		return len(candidates), nil
	}, nmoparams.UncordonTimeout, nmoparams.DefaultPollInterval).Should(BeNumerically(">", 0),
		"no eligible schedulable non-control-plane worker node found (excluding %v)", excludeNodes)

	return selected
}

// isCRDInstalled reports whether the named CRD exists. It fails closed on unexpected
// errors: an API failure must not be mistaken for "not installed" and silently skip a
// spec that requires the CRD.
func isCRDInstalled(ctx context.Context, crdName string) bool {
	GinkgoHelper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := APIClient.Get(ctx, client.ObjectKey{Name: crdName}, crd)
	if err == nil {
		return true
	}

	if errors.IsNotFound(err) {
		return false
	}

	Fail(fmt.Sprintf("checking CRD %s: %v", crdName, err))

	return false
}

// newNodeMaintenance builds a NodeMaintenance CR for the given name and node using the
// shared collision-test reason.
func newNodeMaintenance(name, nodeName string) *nmov1beta1.NodeMaintenance {
	return newNodeMaintenanceWithReason(name, nodeName, nmoparams.CollisionReason)
}

// newNodeMaintenanceWithReason builds a NodeMaintenance CR recording why the test put the node
// under maintenance. The reason surfaces on the CR and in NMO's events, so each scenario
// supplies its own rather than sharing one.
func newNodeMaintenanceWithReason(name, nodeName, reason string) *nmov1beta1.NodeMaintenance {
	return &nmov1beta1.NodeMaintenance{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: nmov1beta1.NodeMaintenanceSpec{
			NodeName: nodeName,
			Reason:   reason,
		},
	}
}

// waitForMaintenanceSucceeded blocks until the named NodeMaintenance reaches the Succeeded
// phase, re-fetching the CR each poll and failing fast on unexpected errors.
func waitForMaintenanceSucceeded(ctx context.Context, name string) {
	GinkgoHelper()

	Eventually(func() (nmov1beta1.MaintenancePhase, error) {
		current := &nmov1beta1.NodeMaintenance{}
		if err := APIClient.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
			return "", fmt.Errorf("getting NodeMaintenance %s: %w", name, err)
		}

		return current.Status.Phase, nil
	}, nmoparams.MaintenanceTimeout, nmoparams.DefaultPollInterval).Should(Equal(nmov1beta1.MaintenanceSucceeded),
		"NodeMaintenance %s did not reach Succeeded phase", name)
}

// assertMaintenanceSucceeded verifies a node has fully entered maintenance, mirroring the
// Python is_maintenance_succeeded helper: the NodeMaintenance reaches Succeeded, the node is
// cordoned and carries the drain taint, and the drain has completed (all pods evicted). This
// also guarantees the node is Unschedulable, which is what lets a subsequent
// selectSchedulableWorker deterministically pick a different node for a name-collision test.
func assertMaintenanceSucceeded(ctx context.Context, name, nodeName string) {
	GinkgoHelper()
	waitForMaintenanceSucceeded(ctx, name)
	assertNodeCordonAndTaint(nodeName, true, nmoparams.MaintenanceTimeout)
	assertDrainCompleted(ctx, name)
}

// deleteNMBestEffort deletes the named NodeMaintenance and waits until it is gone. It is safe
// to call from cleanup: it uses wait.PollUntilContextTimeout (never Fail/panic) and retries
// through any transient (non-NotFound) API error until the timeout, logging a warning, so a
// single blip on a busy cluster does not abandon the deletion. It reports whether the
// deletion was confirmed, so callers can surface an unconfirmed cleanup in the spec report.
func deleteNMBestEffort(ctx context.Context, name string) bool {
	return deleteObjectBestEffort(ctx,
		&nmov1beta1.NodeMaintenance{ObjectMeta: metav1.ObjectMeta{Name: name}},
		"NodeMaintenance "+name)
}

// deleteObjectBestEffort deletes the given object and waits until it is gone. It is safe
// to call from cleanup for the same reasons as deleteNMBestEffort, and generalizes it to
// any typed object, so fixture resources (Deployments, PodDisruptionBudgets) share one
// retry loop instead of each test growing its own variant. It reports whether the deletion
// was confirmed within the timeout.
func deleteObjectBestEffort(ctx context.Context, obj client.Object, description string) bool {
	return deleteObjectBestEffortWithTimeout(ctx, obj, description, nmoparams.CRDeletionTimeout)
}

// deleteObjectBestEffortWithTimeout is deleteObjectBestEffort with an explicit deletion
// budget, for objects whose teardown can outlast the generic CRDeletionTimeout (CRD
// deletion waits on finalizer-driven CR garbage collection). Like deleteObjectBestEffort
// every failed attempt is logged, so a persistent error is visible in the output instead
// of silently burning the budget.
func deleteObjectBestEffortWithTimeout(
	ctx context.Context, obj client.Object, description string, timeout time.Duration,
) bool {
	waitErr := wait.PollUntilContextTimeout(ctx, nmoparams.DefaultPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if delErr := APIClient.Delete(ctx, obj); delErr != nil && !errors.IsNotFound(delErr) {
				GinkgoWriter.Printf("WARNING: deleting %s failed, retrying: %v\n", description, delErr)

				return false, nil
			}

			getErr := APIClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			if getErr != nil && !errors.IsNotFound(getErr) {
				GinkgoWriter.Printf("WARNING: checking %s after delete failed, retrying: %v\n", description, getErr)

				return false, nil
			}

			return errors.IsNotFound(getErr), nil
		})
	if waitErr != nil {
		GinkgoWriter.Printf("WARNING: %s not confirmed deleted within timeout: %v\n", description, waitErr)

		return false
	}

	return true
}

// waitForNodeRecoveryBestEffort waits until the node is Ready, uncordoned, and free of the
// drain taint. Like deleteNMBestEffort it is cleanup-safe (wait.PollUntilContextTimeout, warn
// only) and retries through any transient error until the timeout.
func waitForNodeRecoveryBestEffort(ctx context.Context, nodeName string) {
	// The result is deliberately ignored: this warn-only variant exists for
	// callers that do not surface recovery failures, and the callee already
	// logs every failure it swallows.
	_ = waitForNodeRecoveryWithTimeoutBestEffort(ctx, nodeName, nmoparams.UncordonTimeout)
}

// waitForNodeRecoveryWithTimeoutBestEffort is waitForNodeRecoveryBestEffort with an explicit
// budget, for callers whose failure path deserves the same timeout as the happy path: a
// worker recovering from a stopped kubelet needs the reboot-scale budget, not the
// uncordon-scale one. It returns the wait error so callers can surface an incomplete
// recovery in the spec report.
func waitForNodeRecoveryWithTimeoutBestEffort(
	ctx context.Context, nodeName string, timeout time.Duration,
) error {
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
		GinkgoWriter.Printf("WARNING: node %s did not fully recover (Ready, uncordoned, untainted) within timeout: %v\n",
			nodeName, waitErr)
	}

	return waitErr
}

// cleanupCollisionNMs deletes the given NodeMaintenance CRs and waits for the target node to
// return to a Ready, uncordoned state. All steps are best-effort (no Fail/panic) so a slow
// node recovery cannot leave earlier deletions unattempted. This mirrors the Python
// teardown_method, which deletes every NodeMaintenance object left by the test.
func cleanupCollisionNMs(ctx context.Context, nodeName string, names ...string) {
	for _, name := range names {
		deleteNMBestEffort(ctx, name)
	}

	if nodeName != "" {
		waitForNodeRecoveryBestEffort(ctx, nodeName)
	}
}
