package tests

import (
	"context"
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	manualHealthOwnerLabel        = "test.medik8s.io/manual-health-confirmation"
	manualHealthWitnessAnnotation = "test.medik8s.io/preserve-manual-health"
	manualHealthOwner             = "1973"
	manualHealthNHCName           = "nhc-test-manual-confirmation"
	manualHealthCondition         = "NHCManualHealthTest"
	manualHealthAnnotation        = "remediation.medik8s.io/manually-confirmed-healthy"
	manualHealthKind              = "ManualHealthRemediation"
	manualHealthTemplateKind      = manualHealthKind + "Template"
	manualHealthPlural            = "manualhealthremediations"
	manualHealthTemplatePlural    = "manualhealthremediationtemplates"
)

// manualHealthFixture owns only its dedicated resources and fields on one real Node.
type manualHealthFixture struct {
	ctx       context.Context
	node      *corev1.Node
	claimed   bool
	nhc       *unstructured.Unstructured
	resources []client.Object
}

var _ = Describe("NHC manual health confirmation", Ordered, Serial, func() {
	var fixture *manualHealthFixture

	BeforeAll(func() {
		By("Verifying NHC is Ready and removing owned leftovers")
		verifyNHCDeploymentReady()
		fixture = &manualHealthFixture{ctx: context.Background()}
		fixture.removeLeftovers()

		By("Selecting an isolated Ready worker")
		configs := &unstructured.UnstructuredList{}
		configs.SetGroupVersionKind(nhcGVK.GroupVersion().WithKind("NodeHealthCheckList"))
		Expect(APIClient.List(fixture.ctx, configs)).To(Succeed(), "Existing NHCs must be readable")
		if len(configs.Items) != 0 {
			Skip("Remove other NHC configurations before running manual-health confirmation coverage")
		}
		count, err := helpers.CountReadyWorkerNodes(fixture.ctx, APIClient)
		Expect(err).ToNot(HaveOccurred(), "Ready workers must be readable")
		if count == 0 {
			Skip("Manual-health confirmation coverage requires a Ready, schedulable worker")
		}
		fixture.node, err = helpers.SelectWorkerNode(fixture.ctx, APIClient)
		Expect(err).ToNot(HaveOccurred(), "A Ready worker must be available")
		Expect(fixture.node.Labels).ToNot(HaveKey(manualHealthOwnerLabel), "Worker ownership marker must be unused")
		Expect(fixture.node.Labels["kubernetes.io/hostname"]).ToNot(BeEmpty(), "Worker hostname label must be set")
		Expect(fixture.node.Annotations).ToNot(HaveKey(manualHealthAnnotation), "Existing confirmation must not be consumed")
		Expect(fixture.node.Annotations).ToNot(HaveKey(manualHealthWitnessAnnotation), "Witness annotation must be unused")
		for _, condition := range fixture.node.Status.Conditions {
			Expect(string(condition.Type)).ToNot(Equal(manualHealthCondition), "Test condition must be unused")
		}
	})

	It("Verify NHC clears manual health confirmation after delayed recovery", reportxml.ID("1973"),
		Label(
			labels.OperatorNHC,
			labels.TierAcceptance,
			labels.ComponentController,
			labels.DisruptionNonDestructive,
			labels.PlatformAny,
			labels.FrequencyNightly,
			labels.TopologyMinimalWorker,
		), func() {
			DeferCleanup(func() {
				By("Restoring the worker and removing test resources")
				Expect(fixture.cleanup()).To(Succeed(), "Manual-health test resources and Node fields must be restored")
			})

			By("Creating a harmless remediator and an indefinitely delayed NHC")
			fixture.createResources()
			Expect(waitForNHCPhase(fixture.ctx, manualHealthNHCName, nhcparams.NHCPhaseEnabled,
				nhcparams.ManualHealthTimeout)).To(Succeed(), "Test NHC must initially be Enabled")

			By("Triggering remediation with a test-only Node condition")
			Expect(fixture.setCondition("True")).To(Succeed(), "Only the test condition must become unhealthy")
			var remediationUID types.UID
			Eventually(func() error {
				crs, err := fixture.remediations()
				if err != nil {
					return err
				}
				if len(crs) != 1 {
					return fmt.Errorf("expected one harmless remediation, found %d", len(crs))
				}
				remediationUID = crs[0].GetUID()

				return nil
			}, nhcparams.ManualHealthTimeout, nhcparams.DefaultPollInterval).Should(Succeed())
			Expect(waitForNHCPhase(fixture.ctx, manualHealthNHCName, nhcparams.NHCPhaseRemediating,
				nhcparams.ManualHealthTimeout)).To(Succeed(), "Test NHC must enter Remediating")

			By("Recovering the test condition and verifying manual confirmation is required")
			Expect(fixture.setCondition("False")).To(Succeed(), "Test condition must recover without changing Ready")
			Eventually(fixture.recoveryObserved, nhcparams.ManualHealthTimeout,
				nhcparams.DefaultPollInterval).Should(Succeed(), "NHC must observe recovery while retaining remediation")
			Consistently(func(check Gomega) {
				crs, err := fixture.remediations()
				check.Expect(err).ToNot(HaveOccurred())
				check.Expect(crs).To(HaveLen(1), "Indefinite delay must retain the remediation")
				if len(crs) == 1 {
					check.Expect(crs[0].GetUID()).To(Equal(remediationUID), "The same remediation must remain")
				}
				check.Expect(fixture.recoveryObserved()).To(Succeed())
				check.Expect(fixture.workerPreserved()).To(Succeed())
			}, nhcparams.ManualHealthHoldDuration, nhcparams.DefaultPollInterval).Should(Succeed())

			By("Manually confirming health and waiting for controller cleanup")
			Expect(fixture.patchNode(map[string]interface{}{"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{manualHealthAnnotation: "true"},
			}})).To(Succeed(), "Manual confirmation must be set for the controller to consume")
			Eventually(fixture.completed, nhcparams.ManualHealthTimeout,
				nhcparams.DefaultPollInterval).Should(Succeed(), "NHC must remove the CR and annotation and become Enabled")

			By("Verifying the worker and unrelated metadata were preserved")
			Expect(fixture.workerPreserved()).To(Succeed())
			AddReportEntry("Manual health recovery", fmt.Sprintf("node=%s uid=%s bootID=%s remediation=%s, annotation removed",
				fixture.node.Name, fixture.node.UID, fixture.node.Status.NodeInfo.BootID, remediationUID))
		})
})

func manualHealthObjects(serviceAccount string) []client.Object {
	group, version := nhcparams.TestRemediationGroup, nhcparams.TestRemediationVersion
	template := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": group + "/" + version, "kind": manualHealthTemplateKind,
		"metadata": map[string]interface{}{"name": manualHealthNHCName},
		"spec": map[string]interface{}{"template": map[string]interface{}{
			"spec": map[string]interface{}{"testCase": "RHWA-1973"},
		}},
	}}
	nhc := &unstructured.Unstructured{}
	nhc.SetGroupVersionKind(nhcGVK)
	nhc.SetName(manualHealthNHCName)

	return []client.Object{
		buildTestCRD(manualHealthTemplatePlural+"."+group, manualHealthTemplateKind,
			manualHealthTemplatePlural, "manualhealthremediationtemplate", "nhcmhrt"),
		buildTestCRD(manualHealthPlural+"."+group, manualHealthKind, manualHealthPlural, "manualhealthremediation", "nhcmhr"),
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: manualHealthNHCName}, Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{group},
			Resources: []string{manualHealthPlural, manualHealthPlural + "/status", manualHealthTemplatePlural},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}}},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: manualHealthNHCName},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: serviceAccount, Namespace: medik8sparams.OperatorNs}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: manualHealthNHCName},
		},
		template, nhc,
	}
}

func (fixture *manualHealthFixture) createResources() {
	Expect(fixture.patchNode(map[string]interface{}{"metadata": map[string]interface{}{
		"labels":      map[string]interface{}{manualHealthOwnerLabel: manualHealthOwner},
		"annotations": map[string]interface{}{manualHealthWitnessAnnotation: manualHealthOwner},
	}})).To(Succeed(), "Worker must be marked as owned before adding test state")
	fixture.claimed = true
	deployment, err := APIClient.Deployments(medik8sparams.OperatorNs).Get(fixture.ctx,
		nhcparams.OperatorDeploymentName, metav1.GetOptions{})
	Expect(err).ToNot(HaveOccurred(), "NHC deployment must be readable")
	serviceAccount := deployment.Spec.Template.Spec.ServiceAccountName
	Expect(serviceAccount).ToNot(BeEmpty(), "NHC controller ServiceAccount must be known")
	AddReportEntry("NHC controller", fmt.Sprintf("serviceAccount=%s containers=%v", serviceAccount,
		deployment.Spec.Template.Spec.Containers[0].Image))
	objects := manualHealthObjects(serviceAccount)
	for _, object := range objects {
		object.SetLabels(map[string]string{manualHealthOwnerLabel: manualHealthOwner})
		if object.GetName() == manualHealthNHCName {
			if candidate, ok := object.(*unstructured.Unstructured); ok && candidate.GetKind() == "NodeHealthCheck" {
				fixture.configureNHC(candidate)
			}
		}
		Expect(APIClient.Create(fixture.ctx, object)).To(Succeed(), "Test resource %q must be created", object.GetName())
		fixture.resources = append(fixture.resources, object)
		if crd, ok := object.(*apiextensionsv1.CustomResourceDefinition); ok {
			waitForCRDEstablished(fixture.ctx, crd.GetName())
		}
	}
}

func (fixture *manualHealthFixture) configureNHC(nhc *unstructured.Unstructured) {
	base := buildNHC(manualHealthNHCName, "", "", map[string]interface{}{
		"kubernetes.io/hostname": fixture.node.Labels["kubernetes.io/hostname"],
	})
	spec := nhcSpec(base)
	spec["minHealthy"] = int64(0)
	spec["healthyDelay"] = "-1s"
	spec["unhealthyConditions"] = []interface{}{map[string]interface{}{
		"type": manualHealthCondition, "status": "True", "duration": nhcparams.TestRemediationUnhealthyDuration,
	}}
	spec["remediationTemplate"] = map[string]interface{}{
		"apiVersion": nhcparams.TestRemediationGroup + "/" + nhcparams.TestRemediationVersion,
		"kind":       manualHealthTemplateKind, "name": manualHealthNHCName,
	}
	nhc.Object["spec"] = spec
	fixture.nhc = nhc
}

func (fixture *manualHealthFixture) patchNode(patch map[string]interface{}) error {
	metadata, ok := patch["metadata"].(map[string]interface{})
	if !ok {
		metadata = map[string]interface{}{}
		patch["metadata"] = metadata
	}
	metadata["uid"] = string(fixture.node.UID)
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = APIClient.CoreV1Interface.Nodes().Patch(fixture.ctx, fixture.node.Name, types.StrategicMergePatchType,
		data, metav1.PatchOptions{})

	return err
}

func (fixture *manualHealthFixture) setCondition(status string) error {
	condition := map[string]interface{}{"type": manualHealthCondition, "$patch": "delete"}
	if status != "" {
		now := metav1.Now()
		condition = map[string]interface{}{
			"type": manualHealthCondition, "status": status, "reason": "RegressionTest",
			"message": "RHWA-1973 harmless NHC condition", "lastHeartbeatTime": now, "lastTransitionTime": now,
		}
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{"uid": string(fixture.node.UID)},
		"status":   map[string]interface{}{"conditions": []interface{}{condition}},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = APIClient.CoreV1Interface.Nodes().Patch(fixture.ctx, fixture.node.Name, types.StrategicMergePatchType,
		data, metav1.PatchOptions{}, "status")

	return err
}

func (fixture *manualHealthFixture) remediations() ([]unstructured.Unstructured, error) {
	list, err := APIClient.Resource(schema.GroupVersionResource{
		Group: nhcparams.TestRemediationGroup, Version: nhcparams.TestRemediationVersion, Resource: manualHealthPlural,
	}).List(fixture.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, remediation := range list.Items {
		owned := false
		for _, owner := range remediation.GetOwnerReferences() {
			if owner.Kind == "NodeHealthCheck" && owner.Name == manualHealthNHCName &&
				owner.APIVersion == nhcGVK.GroupVersion().String() &&
				(fixture.nhc == nil || owner.UID == fixture.nhc.GetUID()) {
				owned = true
			}
		}
		if !owned {
			return nil, fmt.Errorf("unowned harmless remediation %q must not be touched", remediation.GetName())
		}
	}

	return list.Items, nil
}

func (fixture *manualHealthFixture) currentNHC() (*unstructured.Unstructured, error) {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(nhcGVK)
	err := APIClient.Get(fixture.ctx, client.ObjectKey{Name: manualHealthNHCName}, current)

	return current, err
}

func (fixture *manualHealthFixture) recoveryObserved() error {
	current, err := fixture.currentNHC()
	if err != nil {
		return err
	}
	phase, _, _ := unstructured.NestedString(current.Object, "status", "phase")
	unhealthy, _, err := unstructured.NestedSlice(current.Object, "status", "unhealthyNodes")
	if err != nil || len(unhealthy) != 1 || phase != nhcparams.NHCPhaseRemediating {
		return fmt.Errorf("NHC must retain one recovering node while Remediating: %v", current.Object["status"])
	}
	entry, ok := unhealthy[0].(map[string]interface{})
	if !ok || entry["name"] != fixture.node.Name || entry["conditionsHealthyTimestamp"] == nil {
		return fmt.Errorf("NHC has not observed recovered test condition: %v", unhealthy)
	}

	return nil
}

func (fixture *manualHealthFixture) completed() error {
	crs, err := fixture.remediations()
	if err != nil {
		return err
	}
	if len(crs) != 0 {
		return fmt.Errorf("NHC still has %d remediations", len(crs))
	}
	node, err := APIClient.CoreV1Interface.Nodes().Get(fixture.ctx, fixture.node.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if _, found := node.Annotations[manualHealthAnnotation]; found {
		return fmt.Errorf("NHC must remove annotation %s from %s using its own RBAC", manualHealthAnnotation, node.Name)
	}
	current, err := fixture.currentNHC()
	if err != nil {
		return err
	}
	phase, _, _ := unstructured.NestedString(current.Object, "status", "phase")
	healthy, _, _ := unstructured.NestedInt64(current.Object, "status", "healthyNodes")
	observed, _, _ := unstructured.NestedInt64(current.Object, "status", "observedNodes")
	unhealthy, _, _ := unstructured.NestedSlice(current.Object, "status", "unhealthyNodes")
	if phase != nhcparams.NHCPhaseEnabled || healthy != 1 || observed != 1 || len(unhealthy) != 0 {
		return fmt.Errorf("NHC must be Enabled with one healthy node: %v", current.Object["status"])
	}

	return nil
}

func (fixture *manualHealthFixture) workerPreserved() error {
	node, err := APIClient.CoreV1Interface.Nodes().Get(fixture.ctx, fixture.node.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if node.UID != fixture.node.UID || node.Status.NodeInfo.BootID != fixture.node.Status.NodeInfo.BootID ||
		!helpers.IsNodeReady(node) || node.Labels[manualHealthOwnerLabel] != manualHealthOwner ||
		node.Annotations[manualHealthWitnessAnnotation] != manualHealthOwner {
		return fmt.Errorf("worker %s must remain Ready with the same UID, boot ID, and witness metadata", node.Name)
	}
	if _, found := node.Annotations[manualHealthAnnotation]; found {
		return fmt.Errorf("unexpected manual-health annotation remains on worker %s", node.Name)
	}

	return nil
}

func (fixture *manualHealthFixture) removeLeftovers() {
	for _, object := range manualHealthObjects(nhcparams.NHCControllerServiceAccount) {
		err := APIClient.Get(fixture.ctx, client.ObjectKeyFromObject(object), object)
		if k8serrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		Expect(err).ToNot(HaveOccurred(), "Reserved test resources must be readable")
		if object.GetLabels()[manualHealthOwnerLabel] != manualHealthOwner {
			Skip(fmt.Sprintf("Reserved resource %q is not owned by RHWA-1973, leaving it untouched", object.GetName()))
		}
		fixture.resources = append(fixture.resources, object)
		if nhc, ok := object.(*unstructured.Unstructured); ok && nhc.GetKind() == "NodeHealthCheck" {
			fixture.nhc = nhc
		}
	}
	nodes, err := APIClient.CoreV1Interface.Nodes().List(fixture.ctx,
		metav1.ListOptions{LabelSelector: manualHealthOwnerLabel + "=" + manualHealthOwner})
	Expect(err).ToNot(HaveOccurred(), "Owned worker markers must be readable")
	Expect(len(nodes.Items)).To(BeNumerically("<=", 1), "Only one worker may carry this test's ownership marker")
	if len(nodes.Items) == 1 {
		fixture.node = &nodes.Items[0]
		fixture.claimed = true
	}
	if len(fixture.resources) > 0 || fixture.claimed {
		Expect(fixture.cleanup()).To(Succeed(), "Marked leftovers must be removed before this run")
	}
	fixture.resources = nil
	fixture.nhc = nil
	fixture.node = nil
	fixture.claimed = false
	fixture.ctx = context.Background()
}

func (fixture *manualHealthFixture) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), nhcparams.ManualHealthCleanupTimeout)
	defer cancel()
	fixture.ctx = ctx
	if fixture.claimed {
		if err := fixture.setCondition(""); err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
		if err := fixture.patchNode(map[string]interface{}{"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{manualHealthAnnotation: nil},
		}}); err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
	}
	crs, err := fixture.remediations()
	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}
	for index := range crs {
		cr := &crs[index]
		cr.SetGroupVersionKind(schema.GroupVersionKind{
			Group: nhcparams.TestRemediationGroup, Version: nhcparams.TestRemediationVersion, Kind: manualHealthKind,
		})
		if err := fixture.deleteObject(cr); err != nil {
			return err
		}
	}
	// Reverse creation order removes NHC before its template, RBAC, and dedicated CRDs.
	for index := len(fixture.resources) - 1; index >= 0; index-- {
		if err := fixture.deleteObject(fixture.resources[index]); err != nil {
			return err
		}
	}
	if fixture.claimed {
		err := fixture.patchNode(map[string]interface{}{"metadata": map[string]interface{}{
			"labels":      map[string]interface{}{manualHealthOwnerLabel: nil},
			"annotations": map[string]interface{}{manualHealthWitnessAnnotation: nil},
		}})
		if k8serrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	return nil
}

func (fixture *manualHealthFixture) deleteObject(object client.Object) error {
	uid := object.GetUID()
	if uid == "" {
		return nil
	}
	var lastError error
	err := wait.PollUntilContextTimeout(fixture.ctx, nhcparams.DefaultPollInterval,
		nhcparams.ManualHealthCleanupTimeout, true, func(ctx context.Context) (bool, error) {
			err := APIClient.Delete(ctx, object, client.Preconditions{UID: &uid},
				client.PropagationPolicy(metav1.DeletePropagationBackground))
			if k8serrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				return true, nil
			}
			if err != nil {
				lastError = err

				return false, nil
			}
			err = APIClient.Get(ctx, client.ObjectKeyFromObject(object), object)
			if k8serrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				return true, nil
			}
			lastError = err

			return false, nil
		})
	if err != nil {
		return fmt.Errorf("delete owned %q: %w (last API error: %w)", object.GetName(), err, lastError)
	}

	return nil
}
