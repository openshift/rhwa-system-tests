package nmoutils

import (
	"fmt"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ExcludeRemediationLabel is applied by NMO while a node is under maintenance.
const ExcludeRemediationLabel = "remediation.medik8s.io/exclude-from-remediation"

// VerifyOwned prevents mutation or cleanup of a replaced or unowned maintenance CR.
func VerifyOwned(object *unstructured.Unstructured, uid types.UID, token, nodeName string) error {
	node, _, err := unstructured.NestedString(object.Object, "spec", "nodeName")
	if err != nil {
		return err
	}
	if uid == "" || object.GetUID() != uid || object.GetLabels()[helpers.FBCRunLabel] != token || node != nodeName {
		return fmt.Errorf("refusing mutation of unowned/replaced NodeMaintenance %s", object.GetName())
	}

	return nil
}

// LastUpdate reads the controller's timestamp; NMO has no observedGeneration field.
func LastUpdate(object *unstructured.Unstructured) (time.Time, error) {
	value, _, err := unstructured.NestedString(object.Object, "status", "lastUpdate")
	if err != nil {
		return time.Time{}, err
	}

	return time.Parse(time.RFC3339Nano, value)
}

// VerifyMaintenance requires completed drain and, when requested, fresh controller status.
// LastError can remain populated after a successful retry; it is not a current phase.
func VerifyMaintenance(object *unstructured.Unstructured, since time.Time) error {
	status, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil {
		return err
	}
	if !found || status["phase"] != "Succeeded" || status["drainProgress"] != int64(nmoparams.DrainProgressComplete) {
		return fmt.Errorf("NMO drain is not complete: %v", status)
	}
	for _, field := range []string{"pendingPods", "pendingPodsRefs"} {
		pods, _, err := unstructured.NestedSlice(object.Object, "status", field)
		if err != nil {
			return err
		}
		if len(pods) != 0 {
			return fmt.Errorf("NMO still reports %s: %v", field, pods)
		}
	}
	updated, err := LastUpdate(object)
	if err != nil {
		return err
	}
	if !since.IsZero() && updated.Before(since) {
		return fmt.Errorf("NMO status is stale: %s is not newer than %s", updated, since)
	}

	return nil
}

// VerifyNode checks the same Ready worker is in maintenance or fully released.
func VerifyNode(node *corev1.Node, uid types.UID, maintenance bool) error {
	_, worker := node.Labels["node-role.kubernetes.io/worker"]
	_, master := node.Labels["node-role.kubernetes.io/master"]
	_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
	if uid == "" || node.UID != uid || !worker || master || controlPlane || !helpers.IsNodeReady(node) {
		return fmt.Errorf("target is not the original Ready worker: %s (UID %s)", node.Name, node.UID)
	}
	drainTaint := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == nmoparams.DrainTaintKey {
			if !maintenance || taint.Effect != corev1.TaintEffectNoSchedule {
				return fmt.Errorf("unexpected drain taint on %s: %v", node.Name, taint)
			}
			drainTaint = true
		}
		if !maintenance && taint.Key == corev1.TaintNodeUnschedulable {
			return fmt.Errorf("worker %s still has the unschedulable taint", node.Name)
		}
	}
	excluded, hasLabel := node.Labels[ExcludeRemediationLabel]
	if node.Spec.Unschedulable != maintenance || drainTaint != maintenance ||
		(maintenance && excluded != "true") || (!maintenance && hasLabel) {
		return fmt.Errorf("worker %s maintenance state does not match %t", node.Name, maintenance)
	}

	return nil
}
