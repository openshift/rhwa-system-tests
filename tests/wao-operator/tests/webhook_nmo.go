package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/wao-operator/internal/waoparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nmoValidatingWebhook = "vnodemaintenance.kb.io"

	nmStoredName = waoparams.WebhookTestPrefix + "-nm"
	nmDryRunName = waoparams.WebhookTestPrefix + "-nm-dry-run"
)

var _ = Describe(
	"WAO NMO admission",
	Ordered, Serial,
	Label(labels.OperatorWAO, labels.OperatorNMO, labels.ComponentWebhook), func() {
		BeforeAll(func() {
			By("Removing the NodeMaintenance of an earlier run")
			Expect(removeObject(context.Background(), newNodeMaintenance(nmStoredName, waoparams.WebhookTestNodeName))).
				To(Succeed(), "A NodeMaintenance %s from an earlier run must be removable", nmStoredName)

			By("Verifying no node has the name the requests use for a missing node")

			err := APIClient.Get(context.Background(), client.ObjectKey{Name: waoparams.WebhookTestNodeName}, &corev1.Node{})
			Expect(k8serrors.IsNotFound(err)).To(BeTrue(),
				"Node %s must not exist; Get returned: %v", waoparams.WebhookTestNodeName, err)
		})

		It("RHWA-9004: serves NMO admission while NMO is Disabled and while it is Enabled",
			reportxml.ID("9004"),
			Label(
				labels.OperatorWAO,
				labels.OperatorNMO,
				labels.TierAcceptance,
				labels.ComponentWebhook,
				labels.DisruptionNonDestructive,
				labels.PlatformAny,
				labels.FrequencyWeekly,
			), func() {
				ctx := context.Background()
				worker := firstNodeName(ctx, workerRoleLabel)
				controlPlane := firstNodeName(ctx, controlPlaneRoleLabel)

				operandAdmissionPhases(medik8sparams.WAOComponentNMO,
					func(state string) {
						verifyAdmissionCases(ctx, state, nmCreateCases(worker, controlPlane)...)
						verifyNMRulesWithStoredMaintenance(ctx, state, worker, controlPlane)
					},
					func(state string) {
						verifyAdmissionCases(ctx, state, nmCreateCases(worker, controlPlane)...)
					}, storedNodeMaintenanceGone)
			})
	})

// newNodeMaintenance builds a NodeMaintenance. A stored one makes the NMO manager cordon and
// drain the node, so the specs store one only while NMO is Disabled.
func newNodeMaintenance(name, nodeName string) *unstructured.Unstructured {
	return newObject(nmGVK, name, "", map[string]interface{}{
		"nodeName": nodeName,
		"reason":   "WAO admission test, not a real maintenance",
	})
}

// nmCreateCases are the NodeMaintenance requests that need no stored object. A dry-run request
// for a real node is not stored, so no node enters maintenance.
func nmCreateCases(worker, controlPlane string) []admissionCase {
	return []admissionCase{
		{
			desc:    "Creating a NodeMaintenance for a node that does not exist with dry-run",
			request: dryRunCreate(newNodeMaintenance(nmDryRunName, waoparams.WebhookTestNodeName)),
			webhook: nmoValidatingWebhook,
			denials: []string{"invalid nodeName, no node with name " + waoparams.WebhookTestNodeName + " found"},
		},
		{
			desc:    "Creating a NodeMaintenance for a worker node with dry-run",
			request: dryRunCreate(newNodeMaintenance(nmDryRunName, worker)),
		},
		{
			// The control plane request makes NMO admission read the etcd PodDisruptionBudget and
			// the etcd guard Pods, so an admitted request also shows those reads are permitted.
			desc:    "Creating a NodeMaintenance for a control plane node with dry-run",
			request: dryRunCreate(newNodeMaintenance(nmDryRunName, controlPlane)),
		},
	}
}

// verifyNMRulesWithStoredMaintenance checks the rules that compare a request with a stored
// NodeMaintenance. NMO is Disabled, so no manager acts on the stored object and the worker stays
// schedulable. The object is removed before the function returns.
func verifyNMRulesWithStoredMaintenance(ctx context.Context, state, worker, controlPlane string) {
	GinkgoHelper()
	stored := newNodeMaintenance(nmStoredName, worker)

	DeferCleanup(func() {
		By("DeferCleanup: removing the stored NodeMaintenance")
		removeObjectsInCleanup(stored)
	})

	By("Storing a NodeMaintenance for a worker node while NMO is Disabled")
	Expect(APIClient.Create(ctx, stored.DeepCopy())).To(Succeed(), "Failed to create NodeMaintenance %s", nmStoredName)

	verifyAdmissionCases(ctx, state,
		admissionCase{
			desc:    "Creating a second NodeMaintenance for the same worker node with dry-run",
			request: dryRunCreate(newNodeMaintenance(nmDryRunName, worker)),
			webhook: nmoValidatingWebhook,
			denials: []string{fmt.Sprintf("a NodeMaintenance for node %s already exists", worker)},
		},
		admissionCase{
			desc: "Updating the node name of the stored NodeMaintenance with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, controlPlane, "spec", "nodeName")
			}),
			webhook: nmoValidatingWebhook,
			denials: []string{"updating spec.NodeName isn't allowed"},
		},
		admissionCase{
			desc: "Updating the reason of the stored NodeMaintenance with dry-run",
			request: dryRunUpdate(stored, func(object *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(object.Object, "changed", "spec", "reason")
			}),
		})

	By("Verifying the worker node of the stored NodeMaintenance is still schedulable")

	node := &corev1.Node{}
	Expect(APIClient.Get(ctx, client.ObjectKey{Name: worker}, node)).To(Succeed(), "Failed to read node %s", worker)
	Expect(node.Spec.Unschedulable).To(BeFalse(),
		"Node %s must stay schedulable: NMO is Disabled, so no manager may cordon it", worker)

	By("Deleting the stored NodeMaintenance")
	Expect(removeObject(ctx, stored)).To(Succeed(),
		"NodeMaintenance %s must be gone before NMO is enabled", nmStoredName)
}

// storedNodeMaintenanceGone reports whether the stored NodeMaintenance is absent. The cleanup
// enables NMO again only when it is: an enabled NMO manager would drain the node of a stored
// NodeMaintenance.
func storedNodeMaintenanceGone() bool {
	stored := newNodeMaintenance(nmStoredName, "")

	return k8serrors.IsNotFound(APIClient.Get(context.Background(), client.ObjectKeyFromObject(stored), stored))
}
