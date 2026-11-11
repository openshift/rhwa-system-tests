package tests

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (test *bpnFixture) createWorkload() {
	GinkgoHelper()
	image, err := test.workloadImage()
	test.prerequisite(err, "the existing workload image digest must resolve before Pod creation")
	test.namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: test.name + "-"}}
	Expect(test.api(func(ctx context.Context) error { return APIClient.Create(ctx, test.namespace) })).To(Succeed())
	config := rest.CopyConfig(APIClient.Config)
	config.Timeout = sbrparams.BPNAPICall
	boundedClient, err := client.New(config, client.Options{})
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNWorkloadReady)
	defer cancel()
	name, err := helpers.CreateWorkloadPod(ctx, boundedClient, test.nodes[0].initial.Name, test.namespace.Name, image,
		"workload-", sbrparams.BPNWorkloadReady, sbrparams.BPNPoll)
	if name != "" {
		test.workload = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: test.namespace.Name}}
	}
	Expect(err).NotTo(HaveOccurred(), "The ordinary pinned workload Pod must become Running and Ready")
	Expect(test.api(func(ctx context.Context) error {
		return APIClient.Get(ctx, client.ObjectKeyFromObject(test.workload), test.workload)
	})).To(Succeed())
	Expect(test.workload.UID).NotTo(BeEmpty())
	Expect(bpnPodReady(test.workload)).To(BeTrue(), "The ordinary pinned workload Pod must be Ready")
	for _, toleration := range test.workload.Spec.Tolerations {
		Expect(toleration.Key).NotTo(Equal("node.kubernetes.io/out-of-service"))
	}
	AddReportEntry("bpn-workload", map[string]interface{}{"name": name, "uid": test.workload.UID, "image": image})
}

func (test *bpnFixture) workloadImage() (string, error) {
	image := medik8sparams.WorkloadImage
	if at := strings.LastIndex(image, "@"); at >= 0 {
		if !bpnValidImageDigest(image[at+1:]) {
			return "", fmt.Errorf("the existing workload image has an invalid sha256 digest")
		}

		return image, nil
	}

	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
	output, err := exec.CommandContext(ctx, "oc", "image", "info", "--filter-by-os=linux/"+
		test.nodes[0].initial.Status.NodeInfo.Architecture, "-o", "json", image).Output()
	cancel()
	if err != nil {
		return "", err
	}
	var info struct {
		Digest string `json:"digest"`
		Name   string `json:"name"`
	}
	if err = json.Unmarshal(output, &info); err != nil {
		return "", err
	}
	if !bpnValidImageDigest(info.Digest) {
		return "", fmt.Errorf("workload image metadata has no valid sha256 digest")
	}
	name := image
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	image = name + "@" + info.Digest

	return image, nil
}

func bpnValidImageDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[len("sha256:"):])

	return err == nil
}

func (test *bpnFixture) releaseRequest(request *unstructured.Unstructured) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNRelease)
	defer cancel()
	Expect(test.releaseRequestWithin(ctx, request)).To(Succeed(),
		"Normal CR deletion and restriction release must finish within one deadline")
	test.quietSince = time.Now()
}

func (test *bpnFixture) releaseRequestWithin(ctx context.Context, request *unstructured.Unstructured) error {
	if err := test.resolveRequestIntent(ctx, request); err != nil {
		return err
	}
	if err := test.deleteRequest(ctx, request); err != nil {
		return err
	}
	var last error
	err := wait.PollUntilContextCancel(ctx, sbrparams.BPNPoll, true, func(call context.Context) (bool, error) {
		last = test.checkRestoredRestrictions(call)

		return last == nil, nil
	})
	if err != nil {
		return fmt.Errorf("restriction release: %w: %w", err, last)
	}

	return nil
}

func (test *bpnFixture) resolveRequestIntent(ctx context.Context, request *unstructured.Unstructured) error {
	if request.GetUID() != "" {
		return nil
	}
	intent := request.GetAnnotations()[bpnRequestIntentAnnotation]
	if intent == "" {
		return fmt.Errorf("request creation has no UID or ownership marker")
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(request.GroupVersionKind())
	err := bpnCleanupAPI(ctx, func(call context.Context) error {
		return APIClient.Get(call, client.ObjectKeyFromObject(request), current)
	})
	// NotFound cannot prove that a timed-out Create will not persist later.
	if err != nil {
		return fmt.Errorf("request creation remains uncertain: %w", err)
	}
	if current.GetUID() == "" || current.GetName() != request.GetName() ||
		current.GetNamespace() != request.GetNamespace() || current.GetAnnotations()[bpnRequestIntentAnnotation] != intent {
		return fmt.Errorf("request creation remains uncertain: the current CR does not match its ownership marker")
	}
	request.SetUID(current.GetUID())

	return nil
}

func (test *bpnFixture) requireRestrictionsReleased() {
	test.eventually(sbrparams.BPNRelease, "Normal deletion must release restrictions", func(ctx context.Context) error {
		return test.checkRestoredRestrictions(ctx)
	})
}

func bpnHasTaint(node *corev1.Node, key, value string, effect corev1.TaintEffect) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == key && taint.Value == value && taint.Effect == effect {
			return true
		}
	}

	return false
}

func (test *bpnFixture) retainPod() {
	Expect(test.api(func(ctx context.Context) error { return APIClient.Delete(ctx, test.workload) })).To(Succeed())
	test.eventually(sbrparams.BPNFaultReady, "The original Pod must remain terminating with its finalizer", test.heldPod)
}

func (test *bpnFixture) heldPod(ctx context.Context) error {
	current := &corev1.Pod{}
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(test.workload), current); err != nil {
		return err
	}
	if current.UID != test.workload.UID || current.DeletionTimestamp == nil {
		return fmt.Errorf("original Pod must remain terminating")
	}
	for _, finalizer := range current.Finalizers {
		if finalizer == "tests.medik8s.io/hold-workload" {
			return nil
		}
	}

	return fmt.Errorf("the test workload finalizer disappeared")
}

func (test *bpnFixture) setAdministratorFixtures(add bool) error {
	return test.api(func(ctx context.Context) error {
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		if add {
			node.Spec.Unschedulable = true
			for _, fixture := range bpnAdministratorTaints() {
				if !bpnHasTaint(node, fixture.Key, fixture.Value, fixture.Effect) {
					node.Spec.Taints = append(node.Spec.Taints, fixture)
				}
			}
		} else {
			node.Spec.Unschedulable = test.nodes[0].initial.Spec.Unschedulable
			remaining := node.Spec.Taints[:0]
			for _, taint := range node.Spec.Taints {
				if bpnOwnedAdministratorTaint(taint, test.nodes[0].initial) {
					continue
				}
				remaining = append(remaining, taint)
			}
			node.Spec.Taints = remaining
		}
		if add {
			test.adminAdded = true
		}
		_, err = APIClient.CoreV1Interface.Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err == nil {
			test.adminAdded = add
		}

		return err
	})
}

func bpnAdministratorTaints() []corev1.Taint {
	return []corev1.Taint{
		{Key: "node.kubernetes.io/out-of-service", Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute},
		{Key: "tests.medik8s.io/retain", Value: "baseline", Effect: corev1.TaintEffectNoSchedule},
	}
}

func bpnOwnedAdministratorTaint(taint corev1.Taint, initial *corev1.Node) bool {
	for _, fixture := range bpnAdministratorTaints() {
		if taint.Key == fixture.Key && taint.Value == fixture.Value && taint.Effect == fixture.Effect &&
			!bpnHasTaint(initial, fixture.Key, fixture.Value, fixture.Effect) {
			return true
		}
	}

	return false
}

func (test *bpnFixture) checkRestoredRestrictions(ctx context.Context) error {
	for index, node := range test.nodes {
		current, err := test.cleanupNode(ctx, index)
		if err != nil {
			return err
		}
		expected := node.initial.DeepCopy()
		if index == 0 && test.adminAdded {
			expected.Spec.Unschedulable = true
			for _, fixture := range bpnAdministratorTaints() {
				if !bpnHasTaint(expected, fixture.Key, fixture.Value, fixture.Effect) {
					expected.Spec.Taints = append(expected.Spec.Taints, fixture)
				}
			}
		}
		sameTaints := bpnSameTaints(current.Spec.Taints, expected.Spec.Taints)
		if current.Spec.Unschedulable != expected.Spec.Unschedulable || !sameTaints {
			return fmt.Errorf("node %s must retain its original cordon and every original taint", current.Name)
		}
	}

	return nil
}

func bpnSameTaints(current, expected []corev1.Taint) bool {
	if len(current) != len(expected) {
		return false
	}
	for _, wanted := range expected {
		found := false
		for _, actual := range current {
			if reflect.DeepEqual(actual, wanted) {
				found = true

				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

type bpnRequestClient struct {
	client.Client
	uid types.UID
}

func (guard bpnRequestClient) Get(ctx context.Context, key client.ObjectKey,
	object client.Object, options ...client.GetOption,
) error {
	return bpnCleanupAPI(ctx, func(call context.Context) error {
		if err := guard.Client.Get(call, key, object, options...); err != nil {
			return err
		}
		if object.GetUID() != guard.uid {
			return fmt.Errorf("CR %s/%s UID changed from %s to %s", key.Namespace, key.Name, guard.uid, object.GetUID())
		}

		return nil
	})
}

func (guard bpnRequestClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if object.GetUID() != guard.uid {
		return fmt.Errorf("refuse to delete a different CR UID")
	}
	options = append(options, client.Preconditions{UID: &guard.uid})

	return bpnCleanupAPI(ctx, func(call context.Context) error { return guard.Client.Delete(call, object, options...) })
}

func (test *bpnFixture) deleteRequest(ctx context.Context, request *unstructured.Unstructured) error {
	if _, released := test.released[request.GetUID()]; released {
		return nil
	}
	if request.GetUID() == "" {
		return fmt.Errorf("the request UID must exist before deletion")
	}
	guard := bpnRequestClient{Client: APIClient, uid: request.GetUID()}
	if !helpers.DeleteRemediationCR(ctx, guard, request.GroupVersionKind(), request.GetName(), request.GetNamespace(),
		sbrparams.BPNPoll, sbrparams.BPNRelease, GinkgoWriter.Printf) {
		return fmt.Errorf("normal deletion of CR UID %s did not finish", request.GetUID())
	}
	test.markRequestReleased(request.GetUID())

	return nil
}

func (test *bpnFixture) markRequestReleased(uid types.UID) {
	if test.released == nil {
		test.released = map[types.UID]time.Time{}
	}
	test.released[uid] = time.Now()
}

func bpnCleanupAPI(ctx context.Context, operation func(context.Context) error) error {
	call, cancel := context.WithTimeout(ctx, sbrparams.BPNAPICall)
	defer cancel()

	return operation(call)
}

func bpnCleanupPoll(timeout time.Duration, operation func(context.Context) error) error {
	var last error
	err := wait.PollUntilContextTimeout(context.Background(), sbrparams.BPNPoll, timeout, true,
		func(ctx context.Context) (bool, error) {
			last = operation(ctx)

			return last == nil, nil
		})
	if err != nil {
		return fmt.Errorf("%w: %w", err, last)
	}

	return nil
}

func (test *bpnFixture) cleanupNode(ctx context.Context, index int) (*corev1.Node, error) {
	var node *corev1.Node
	err := bpnCleanupAPI(ctx, func(call context.Context) error {
		var err error
		node, err = test.getNode(call, index)

		return err
	})

	return node, err
}

func (test *bpnFixture) cleanup() {
	GinkgoHelper()
	test.cleaning = true
	var failures []string
	check := func(label string, operation func() error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				bpnRecoveryFailed = true
				failures = append(failures, fmt.Sprintf("%s: panic: %v", label, recovered))
			}
		}()
		if err := operation(); err != nil {
			bpnRecoveryFailed = true
			failures = append(failures, label+": "+err.Error())
		}
	}
	completed := false
	defer func() {
		if !completed {
			bpnRecoveryFailed = true
		}
		test.finishCleanup(check)
		AddReportEntry("bpn-cleanup", failures)
		if len(failures) > 0 {
			Fail("Cleanup failed. Stop later disruptive cases: " + strings.Join(failures, " | "))
		}
	}()
	if !test.restoreCleanupFaults(check) {
		completed = true

		return
	}
	if test.prepared {
		check("restore storage health", test.restoreStorageHealth)
		for index, node := range test.nodes {
			check("Node Ready "+node.initial.Name, func() error { return test.restoreNode(index) })
		}
	}
	for _, request := range test.requests {
		check("normal CR deletion", func() error { return test.removeCleanupRequest(request) })
	}
	if test.prepared {
		check("SBR restriction release", func() error {
			return bpnCleanupPoll(sbrparams.BPNRelease, test.checkRestoredRestrictions)
		})
	}
	if test.adminAdded {
		check("remove administrator fixtures", func() error { return test.setAdministratorFixtures(false) })
	}
	check("remove test workload", test.removeWorkload)
	test.removeCleanupFixtures(check)
	if test.prepared && len(failures) == 0 {
		check("final restored observation", test.finalRestoredObservation)
	}
	completed = true
}

func (test *bpnFixture) restoreCleanupFaults(check func(string, func() error)) bool {
	releaseRequests := false
	for _, fault := range test.journalFaults {
		if fault.releaseRequestsBeforeRestore {
			releaseRequests = true
		}
	}
	if releaseRequests {
		cancelled := false
		check("cancel pending requests before restoring journal writes", func() error {
			for _, request := range test.requests {
				if err := test.removeCleanupRequest(request); err != nil {
					return err
				}
			}
			cancelled = true

			return nil
		})
		if !cancelled {
			return false
		}
	}
	for _, fault := range test.faults {
		check("remove native network fault", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNFaultReady)
			defer cancel()

			return fault.Restore(ctx)
		})
	}
	for _, fault := range test.journalFaults {
		check("restore journal directory flags", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNFaultReady)
			defer cancel()

			return fault.Restore(ctx)
		})
	}
	check("restore host services and settings", test.restoreHostSettings)

	return true
}

func (test *bpnFixture) removeCleanupFixtures(check func(string, func() error)) {
	owners := map[types.UID]bool{}
	if test.config != nil && test.config.GetUID() != "" {
		captured := false
		check("record configuration ownership", func() error {
			err := test.configurationOwners(owners)
			captured = err == nil

			return err
		})
		if !captured {
			return
		}
	}
	for _, object := range []*unstructured.Unstructured{test.nhc, test.template, test.config} {
		if object != nil && object.GetUID() != "" {
			check("delete "+object.GetKind(), func() error { return test.deleteFixture(object) })
		}
	}
	if len(owners) > 0 {
		check("configuration-owned operands absent", func() error { return test.configurationGone(owners) })
	}
	for index, node := range test.nodes {
		check("restore worker label "+node.initial.Name, func() error { return test.restoreWorkerLabel(index) })
	}
	check("delete workload namespace", test.deleteWorkloadNamespace)
}

func (test *bpnFixture) removeCleanupRequest(request *unstructured.Unstructured) error {
	if !test.nhcRequests[request.GetUID()] {
		ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNRelease)
		defer cancel()

		return test.releaseRequestWithin(ctx, request)
	}

	return bpnCleanupPoll(sbrparams.BPNNHCHealthyDelay+sbrparams.BPNRelease, func(ctx context.Context) error {
		err := bpnCleanupAPI(ctx, func(call context.Context) error {
			_, err := test.requestCurrent(call, request)

			return err
		})
		if apierrors.IsNotFound(err) {
			test.markRequestReleased(request.GetUID())

			return test.checkRestoredRestrictions(ctx)
		}
		if err != nil {
			return err
		}

		return fmt.Errorf("NHC must normally delete its request after healthyDelay")
	})
}

func (test *bpnFixture) restoreHostSettings() error {
	if test.hostSettings == nil {
		return nil
	}

	return test.hostSettings.restore()
}

func (settings *bpnHostSettings) restore() error {
	var failures []string
	if settings.kubeletStopped && settings.host != nil {
		if err := bpnCleanupPoll(sbrparams.BPNRelease, func(ctx context.Context) error {
			_, err := settings.host.Run(ctx, sbrparams.BPNHostCall,
				"sudo systemctl start kubelet && systemctl is-active --quiet kubelet")

			return err
		}); err != nil {
			failures = append(failures, "start kubelet: "+err.Error())
		}
	}
	if settings.sysctlSaved != "" {
		values := strings.Fields(settings.sysctlSaved)
		if len(values) != 2 {
			failures = append(failures, "saved SysRq settings are invalid")
		} else {
			_, err := settings.host.Run(context.Background(), sbrparams.BPNHostCall,
				"sudo sysctl -w kernel.sysrq="+bpnQuote(values[0])+" kernel.panic="+bpnQuote(values[1]))
			if err != nil {
				failures = append(failures, "restore SysRq settings: "+err.Error())
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, " | "))
	}

	return nil
}

func (test *bpnFixture) restoreStorageHealth() error {
	return bpnCleanupPoll(sbrparams.BPNHealthChange, func(ctx context.Context) error {
		for index := range test.nodes {
			node, err := test.cleanupNode(ctx, index)
			if err != nil {
				return err
			}
			if bpnHealth(node) != corev1.ConditionFalse {
				return fmt.Errorf("node %s storage health is %s", node.Name, bpnHealth(node))
			}
		}

		return nil
	})
}

func (test *bpnFixture) restoreNode(index int) error {
	return bpnCleanupPoll(sbrparams.BPNNodeReady, func(ctx context.Context) error {
		node := test.nodes[index]
		current, err := test.cleanupNode(ctx, index)
		if err != nil {
			return err
		}
		if !helpers.IsNodeReady(current) {
			return fmt.Errorf("node %s is not Ready", current.Name)
		}
		boot, err := node.host.BootID(ctx, sbrparams.BPNHostCall)
		if err != nil {
			return err
		}
		if current.Status.NodeInfo.BootID != boot {
			return fmt.Errorf("node %s API and actual host boot IDs differ", current.Name)
		}

		return nil
	})
}

func (test *bpnFixture) removeWorkload() error {
	if test.workload == nil {
		return nil
	}

	return bpnCleanupPoll(sbrparams.BPNWorkloadGone, func(ctx context.Context) error {
		current := &corev1.Pod{}
		err := bpnCleanupAPI(ctx, func(call context.Context) error {
			return APIClient.Get(call, client.ObjectKeyFromObject(test.workload), current)
		})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if test.workload.UID != "" && current.UID != test.workload.UID {
			return fmt.Errorf("workload UID changed")
		}
		finalizers := make([]string, 0, len(current.Finalizers))
		for _, finalizer := range current.Finalizers {
			if finalizer != "tests.medik8s.io/hold-workload" {
				finalizers = append(finalizers, finalizer)
			}
		}
		if len(finalizers) != len(current.Finalizers) {
			current.Finalizers = finalizers
			err = bpnCleanupAPI(ctx, func(call context.Context) error { return APIClient.Update(call, current) })
			if err != nil {
				return err
			}
		}
		uid := current.UID
		if err = bpnCleanupAPI(ctx, func(call context.Context) error {
			return APIClient.Delete(call, current, client.Preconditions{UID: &uid})
		}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		return fmt.Errorf("workload deletion is pending")
	})
}

func (test *bpnFixture) deleteFixture(object *unstructured.Unstructured) error {
	return bpnCleanupPoll(sbrparams.BPNAgentReady, func(ctx context.Context) error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GroupVersionKind())
		err := bpnCleanupAPI(ctx, func(call context.Context) error {
			return APIClient.Get(call, client.ObjectKeyFromObject(object), current)
		})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.GetUID() != object.GetUID() {
			return fmt.Errorf("resource UID changed")
		}
		uid := object.GetUID()
		if err = bpnCleanupAPI(ctx, func(call context.Context) error {
			return APIClient.Delete(call, current, client.Preconditions{UID: &uid})
		}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		return fmt.Errorf("normal deletion remains pending")
	})
}

func (test *bpnFixture) configurationOwners(owners map[types.UID]bool) error {
	owners[test.config.GetUID()] = true
	_, err := test.configurationOperands(context.Background(), owners)

	return err
}

func bpnOwnedBy(references []metav1.OwnerReference, owners map[types.UID]bool) bool {
	for _, reference := range references {
		if owners[reference.UID] {
			return true
		}
	}

	return false
}

func (test *bpnFixture) configurationGone(owners map[types.UID]bool) error {
	return bpnCleanupPoll(sbrparams.BPNAgentReady, func(ctx context.Context) error {
		remaining, err := test.configurationOperands(ctx, owners)
		if err != nil {
			return err
		}
		if len(remaining) > 0 {
			return fmt.Errorf("configuration-owned operands remain: %s", strings.Join(remaining, ", "))
		}

		return nil
	})
}

func (test *bpnFixture) configurationOperands(ctx context.Context, owners map[types.UID]bool) ([]string, error) {
	var remaining []string
	var firstError error
	for _, resource := range []string{"daemonsets", "pods", "persistentvolumeclaims"} {
		if resource == "pods" && test.podRecorder != nil {
			for _, observation := range test.podRecorder.Snapshot().Observations {
				pod := observation.Object
				if pod != nil && pod.Namespace == medik8sparams.OperatorNs && pod.UID != "" &&
					bpnOwnedBy(pod.OwnerReferences, owners) {
					owners[pod.UID] = true
				}
			}
		}
		group, version := "", "v1"
		if resource == "daemonsets" {
			group = "apps"
		}
		gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
		err := bpnCleanupAPI(ctx, func(call context.Context) error {
			objects, listErr := APIClient.Resource(gvr).Namespace(medik8sparams.OperatorNs).List(call, metav1.ListOptions{})
			if listErr != nil {
				return listErr
			}
			for _, object := range objects.Items {
				uid := object.GetUID()
				if uid != "" && (owners[uid] || bpnOwnedBy(object.GetOwnerReferences(), owners)) {
					owners[uid] = true
					remaining = append(remaining, fmt.Sprintf("%s/%s UID %s", resource, object.GetName(), uid))
				}
			}

			return nil
		})
		if err != nil && firstError == nil {
			firstError = err
		}
	}

	return remaining, firstError
}

func (test *bpnFixture) restoreWorkerLabel(index int) error {
	return bpnCleanupPoll(sbrparams.BPNRelease, func(ctx context.Context) error {
		node, err := test.cleanupNode(ctx, index)
		if err != nil {
			return err
		}
		const key = "tests.medik8s.io/block-per-node"
		original, originalExists := test.nodes[index].initial.Labels[key]
		current, currentExists := node.Labels[key]
		if originalExists == currentExists && original == current {
			return nil
		}
		if current != test.name {
			return fmt.Errorf("worker label has an unowned value %q", current)
		}
		if originalExists {
			node.Labels[key] = original
		} else {
			delete(node.Labels, key)
		}

		return bpnCleanupAPI(ctx, func(call context.Context) error {
			_, err := APIClient.CoreV1Interface.Nodes().Update(call, node, metav1.UpdateOptions{})

			return err
		})
	})
}

func (test *bpnFixture) deleteWorkloadNamespace() error {
	if test.namespace == nil || test.namespace.UID == "" {
		return nil
	}

	return bpnCleanupPoll(sbrparams.BPNRelease, func(ctx context.Context) error {
		current := &corev1.Namespace{}
		err := bpnCleanupAPI(ctx, func(call context.Context) error {
			return APIClient.Get(call, client.ObjectKeyFromObject(test.namespace), current)
		})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.UID != test.namespace.UID {
			return fmt.Errorf("workload namespace UID changed")
		}
		uid := test.namespace.UID
		if err = bpnCleanupAPI(ctx, func(call context.Context) error {
			return APIClient.Delete(call, current, client.Preconditions{UID: &uid})
		}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		return fmt.Errorf("workload namespace deletion is pending")
	})
}

func (test *bpnFixture) finalRestoredObservation() error {
	if err := test.checkRestorationBoots(); err != nil {
		return err
	}
	if err := test.finalCleanupBoots(); err != nil {
		return err
	}
	from := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNPostRestore)
	defer cancel()
	for {
		if err := test.checkFinalRestoration(from); err != nil {
			return err
		}
		timer := time.NewTimer(sbrparams.BPNPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			if err := test.finalCleanupBoots(); err != nil {
				return err
			}
			through := time.Now()
			if err := bpnCleanupPoll(sbrparams.BPNObserverStart, func(context.Context) error {
				return test.cleanupCoverage(from, through)
			}); err != nil {
				return err
			}

			return test.checkFinalRestoration(from)
		case <-timer.C:
		}
	}
}

func (test *bpnFixture) finalCleanupBoots() error {
	for _, node := range test.nodes {
		boot, err := node.host.BootID(context.Background(), sbrparams.BPNHostCall)
		if err != nil {
			return err
		}
		if boot != node.boot {
			return fmt.Errorf("node %s rebooted after restoration", node.initial.Name)
		}
	}

	return nil
}

func (test *bpnFixture) cleanupCoverage(from, through time.Time) error {
	if err := test.crRecorder.Coverage(from, through); err != nil {
		return err
	}
	for _, node := range test.nodes {
		if err := node.recorder.Coverage(from, through); err != nil {
			return err
		}
	}

	return nil
}

func (test *bpnFixture) checkFinalRestoration(from time.Time) error {
	if err := test.checkRestorationBoots(); err != nil {
		return err
	}
	if err := test.noRestrictions(from); err != nil {
		return err
	}

	return test.checkRestoredRestrictions(context.Background())
}

func (test *bpnFixture) checkRestorationBoots() error {
	if err := test.checkBoots(test.baseline, 1, 2); err != nil {
		return err
	}
	from := test.baseline
	if !test.nodes[0].rebootObservedAt.IsZero() {
		from = test.nodes[0].rebootObservedAt
	}

	return test.checkBoots(from, 0)
}

func (test *bpnFixture) finishCleanup(check func(string, func() error)) {
	if test.samplingCancel != nil {
		test.samplingCancel()
		if test.samplingDone != nil {
			<-test.samplingDone
		}
	}
	for _, node := range test.nodes {
		if node.recorder != nil {
			check("stop Node observer "+node.initial.Name, func() error {
				node.recorder.Stop()
				AddReportEntry("bpn-node-observations "+node.initial.Name, node.recorder.Snapshot())

				return nil
			})
		}
		if node.host != nil {
			check("close prepared SSH "+node.initial.Name, node.host.Close)
		}
	}
	if test.crRecorder != nil {
		check("stop request observer", func() error {
			test.crRecorder.Stop()
			AddReportEntry("bpn-request-observations", test.crRecorder.Snapshot())

			return nil
		})
	}
	if test.podRecorder != nil {
		check("stop Pod observer", func() error {
			test.podRecorder.Stop()

			return nil
		})
	}
	test.mu.Lock()
	samples := append([]bpnBootSample(nil), test.samples...)
	test.mu.Unlock()
	AddReportEntry("bpn-host-boot-samples", samples)
}

func (test *bpnFixture) holdWorkloadWithFinalizer() {
	test.workload.Finalizers = append(test.workload.Finalizers, "tests.medik8s.io/hold-workload")
	Expect(test.api(func(ctx context.Context) error { return APIClient.Update(ctx, test.workload) })).To(Succeed())
}

type bpnOwnedJournalFault struct {
	*sbrutils.BlockJournalFault
	releaseRequestsBeforeRestore bool
}

type bpnHostSettings struct {
	host           *helpers.PreparedNodeAccess
	kubeletStopped bool
	sysctlSaved    string
}
