package tests

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (test *bpnFixture) createNamespaceAndWaitForPinnedWorkloadPod() {
	GinkgoHelper()
	test.createWorkloadPod(false)
	test.waitForAPICheck(sbrparams.BPNWorkloadReady, "The original pinned Pod must become Running and Ready",
		func(ctx context.Context) error {
			current, err := test.getWorkloadPod(ctx)
			if err != nil {
				return err
			}
			if current.Status.Phase != corev1.PodRunning || !bpnPodReady(current) {
				return fmt.Errorf("the pinned Pod is not Running and Ready")
			}
			test.workload = current

			return nil
		})
	Expect(test.workload.Spec.NodeName).To(Equal(test.nodes[0].initial.Name))
}

func (test *bpnFixture) createWorkloadPod(useScheduler bool) {
	GinkgoHelper()
	image, err := test.resolvePinnedWorkloadImage()
	test.skipOrFailOnPrerequisiteError(err, "the existing workload image digest must resolve before Pod creation")
	if test.namespace == nil {
		test.namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: test.name + "-"}}
		Expect(test.callAPIWithNodeIdentityChecks(
			func(ctx context.Context) error { return APIClient.Create(ctx, test.namespace) },
		)).To(Succeed())
	}
	test.workload = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workload-", Namespace: test.namespace.Name},
		Spec: corev1.PodSpec{
			NodeName:   test.nodes[0].initial.Name,
			Containers: []corev1.Container{{Name: "workload", Image: image, Command: []string{"sleep", "infinity"}}},
			Tolerations: []corev1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
				{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
			},
		},
	}
	if useScheduler {
		test.workload.Spec.NodeName = ""
		test.workload.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{
					Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{test.nodes[0].initial.Name},
				}}}},
			},
		}}
	}
	Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		return APIClient.Create(ctx, test.workload)
	})).To(Succeed())
	Expect(test.workload.UID).NotTo(BeEmpty())
	oos := corev1.Taint{Key: sbrparams.OutOfServiceTaintKey, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	for _, toleration := range test.workload.Spec.Tolerations {
		Expect(toleration.ToleratesTaint(klog.Background(), &oos, false)).To(BeFalse(),
			"The pinned Pod must be eligible for OutOfService eviction")
	}
	AddReportEntry("bpn-workload", map[string]interface{}{
		"name": test.workload.Name, "uid": test.workload.UID, "image": image,
	})
}

func (test *bpnFixture) getWorkloadPod(ctx context.Context) (*corev1.Pod, error) {
	current := &corev1.Pod{}
	if err := APIClient.Get(ctx, client.ObjectKeyFromObject(test.workload), current); err != nil {
		return nil, err
	}
	if current.UID != test.workload.UID {
		return nil, bpnPermanentError{fmt.Errorf("the workload Pod UID changed")}
	}

	return current, nil
}

func (test *bpnFixture) waitForOriginalWorkloadEviction(evidence bpnCleanupEvidence) {
	GinkgoHelper()
	Expect(test.workload).NotTo(BeNil(), "The case must create a pinned workload before remediation")
	Expect(test.workload.UID).NotTo(BeEmpty(), "The test must retain the original workload UID")
	deadline := evidence.startedAt.Add(sbrparams.BPNWorkloadGone)
	test.waitForAPICheck(bpnReplayWait(deadline), "OutOfService must remove the original pinned Pod UID",
		func(_ context.Context) error {
			for _, sample := range test.podRecorder.Snapshot().Observations {
				if sample.UID != test.workload.UID || sample.EventType != watch.Deleted {
					continue
				}
				if sample.At.Before(evidence.startedAt) || sample.At.After(deadline) {
					return bpnPermanentError{
						fmt.Errorf("pod UID %s was deleted outside the OutOfService eviction interval", sample.UID),
					}
				}

				return nil
			}

			return fmt.Errorf("pod UID %s has no recorded Delete event", test.workload.UID)
		})
}

func (test *bpnFixture) resolvePinnedWorkloadImage() (string, error) {
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

func (test *bpnFixture) deleteSBRAndWaitForBaselineRestrictions(request *unstructured.Unstructured) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNRelease)
	defer cancel()
	Expect(test.deleteSBRAndWaitForBaselineRestrictionsWithin(ctx, request)).To(Succeed(),
		"Normal CR deletion and restriction release must finish within one deadline")
	test.quietSince = time.Now()
}

func (test *bpnFixture) waitForNHCToDeleteSBRAndRestoreBaselineRestrictions(
	request *unstructured.Unstructured, healthyAt time.Time,
) {
	GinkgoHelper()
	releaseBy := healthyAt.Add(sbrparams.BPNNHCHealthyDelay + sbrparams.BPNRelease)
	test.waitForAPICheck(time.Until(releaseBy), "NHC must delete and release within 360s",
		func(ctx context.Context) error {
			_, err := test.getAndCheckSBRCR(ctx, request)
			if apierrors.IsNotFound(err) {
				return test.checkRestoredRestrictions(ctx)
			}
			if err != nil {
				return err
			}

			return fmt.Errorf("NHC request remains")
		})
	test.markRequestReleased(request.GetUID())
	test.quietSince = time.Now()
}

func (test *bpnFixture) deleteSBRAndWaitForBaselineRestrictionsWithin(
	ctx context.Context, request *unstructured.Unstructured,
) error {
	if err := test.resolveRequestIntent(ctx, request); err != nil {
		return err
	}
	if err := test.deleteSBRByUIDAndWait(ctx, request); err != nil {
		return err
	}
	var last error
	err := wait.PollUntilContextCancel(ctx, sbrparams.BPNPoll, true, func(call context.Context) (bool, error) {
		last = test.checkRestoredRestrictions(call)
		if bpnIsPermanent(last) {
			return false, last
		}

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

func (test *bpnFixture) waitForNodeRestrictionsToMatchBaseline() {
	test.waitForAPICheck(sbrparams.BPNRelease, "Normal deletion must release restrictions",
		func(ctx context.Context) error {
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

func (test *bpnFixture) deleteWorkloadPodAndWaitForFinalizerHold() {
	Expect(test.callAPIWithNodeIdentityChecks(
		func(ctx context.Context) error { return APIClient.Delete(ctx, test.workload) },
	)).To(Succeed())
	test.waitForAPICheck(
		sbrparams.BPNFaultReady, "The original Pod must remain terminating with its finalizer",
		test.checkWorkloadPodTerminatingWithFinalizer,
	)
}

func (test *bpnFixture) checkWorkloadPodTerminatingWithFinalizer(ctx context.Context) error {
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

func (test *bpnFixture) setAdministratorCordonAndTaints(add bool) error {
	return test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
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
	for index := range test.nodes {
		current, err := test.getNodeForCleanup(ctx, index)
		if err != nil {
			return err
		}
		if err := test.checkNodeRestrictions(index, current); err != nil {
			return err
		}
	}

	return nil
}

func (test *bpnFixture) checkNodeRestrictions(index int, current *corev1.Node) error {
	expected := test.nodes[index].initial.DeepCopy()
	if index == 0 && test.adminAdded {
		expected.Spec.Unschedulable = true
		for _, fixture := range bpnAdministratorTaints() {
			if !bpnHasTaint(expected, fixture.Key, fixture.Value, fixture.Effect) {
				expected.Spec.Taints = append(expected.Spec.Taints, fixture)
			}
		}
	}
	if current.Spec.Unschedulable != expected.Spec.Unschedulable ||
		!bpnSameTaints(current.Spec.Taints, expected.Spec.Taints) {
		return fmt.Errorf("node %s must match the expected cordon and taints", current.Name)
	}

	return bpnCheckClearedRejoin(current)
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

func (test *bpnFixture) deleteSBRByUIDAndWait(ctx context.Context, request *unstructured.Unstructured) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return bpnCleanupPollWithin(ctx, operation)
}

func bpnCleanupPollWithin(ctx context.Context, operation func(context.Context) error) error {
	var last error
	err := wait.PollUntilContextCancel(ctx, sbrparams.BPNPoll, true, func(call context.Context) (bool, error) {
		last = operation(call)
		if bpnIsPermanent(last) {
			return false, last
		}

		return last == nil, nil
	})
	if err != nil {
		return fmt.Errorf("%w: %w", err, last)
	}

	return nil
}

func (test *bpnFixture) getNodeForCleanup(ctx context.Context, index int) (*corev1.Node, error) {
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
		test.stopObserversCloseSSHAndReportEvidence(check)
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
		check("restore storage health", test.waitForHealthyStorageOnAllNodes)
		check("Nodes Ready", func() error {
			return bpnCleanupPoll(sbrparams.BPNNodeReady, test.checkAllNodesReadyAndBootIDsMatch)
		})
	}
	requestsReleased := test.releaseCleanupRequests(check)
	fixturesCanBeRemoved := requestsReleased
	if test.adminAdded {
		check("remove administrator fixtures", func() error { return test.setAdministratorCordonAndTaints(false) })
	}
	if test.prepared && requestsReleased {
		workersRecovered := false
		check("complete worker recovery before removing SBR fixtures", func() error {
			err := bpnCleanupPoll(sbrparams.BPNAgentReady, func(ctx context.Context) error {
				return test.checkWorkerRecovery(ctx, corev1.ConditionFalse)
			})
			workersRecovered = err == nil

			return err
		})
		if test.bootUnit != "" {
			fixturesCanBeRemoved = workersRecovered
		}
	}
	check("remove test workload", test.removeWorkloadFinalizerAndWaitForPodDeletion)
	if fixturesCanBeRemoved {
		test.removeCleanupFixtures(check)
	}
	if test.prepared && len(failures) == 0 {
		check("final restored observation", test.finalRestoredObservation)
	}
	completed = true
}

func (test *bpnFixture) releaseCleanupRequests(check func(string, func() error)) bool {
	requestsReleased := true
	for _, request := range test.requests {
		released := false
		check("normal CR deletion", func() error {
			err := test.removeCleanupRequest(request)
			released = err == nil

			return err
		})
		requestsReleased = requestsReleased && released
	}
	if test.prepared {
		released := false
		check("SBR restriction release", func() error {
			err := bpnCleanupPoll(sbrparams.BPNRelease, test.checkRestoredRestrictions)
			released = err == nil

			return err
		})
		requestsReleased = requestsReleased && released
	}

	return requestsReleased
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
			err := test.recordSBRConfigAndOperandUIDs(owners)
			captured = err == nil

			return err
		})
		if !captured {
			return
		}
	}
	removed := false
	check("delete fixtures and configuration-owned operands", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNAgentReady)
		defer cancel()
		err := test.deleteFixtures(ctx, owners)
		removed = err == nil

		return err
	})
	if removed {
		if test.bootUnit != "" && test.config != nil && test.config.GetUID() != "" {
			check("remove or disable the agent-managed boot unit", test.waitForBootUnitRemoval)
		}
		check("restore worker labels", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNRelease)
			defer cancel()

			return test.restoreWorkerLabels(ctx)
		})
	}
	check("delete workload namespace", test.deleteWorkloadNamespace)
}

func (test *bpnFixture) removeCleanupRequest(request *unstructured.Unstructured) error {
	var pauseRequests []string
	if test.nhc != nil {
		var err error
		pauseRequests, _, err = unstructured.NestedStringSlice(test.nhc.Object, "spec", "pauseRequests")
		if err != nil {
			return err
		}
	}
	if !test.nhcRequests[request.GetUID()] || len(pauseRequests) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), sbrparams.BPNRelease)
		defer cancel()

		return test.deleteSBRAndWaitForBaselineRestrictionsWithin(ctx, request)
	}

	return bpnCleanupPoll(sbrparams.BPNNHCHealthyDelay+sbrparams.BPNRelease, func(ctx context.Context) error {
		err := bpnCleanupAPI(ctx, func(call context.Context) error {
			_, err := test.getAndCheckSBRCR(call, request)

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

func (test *bpnFixture) waitForHealthyStorageOnAllNodes() error {
	return bpnCleanupPoll(sbrparams.BPNHealthChange, func(ctx context.Context) error {
		for index := range test.nodes {
			node, err := test.getNodeForCleanup(ctx, index)
			if err != nil {
				return err
			}
			if bpnStorageUnhealthyStatus(node) != corev1.ConditionFalse {
				return fmt.Errorf("node %s storage health is %s", node.Name, bpnStorageUnhealthyStatus(node))
			}
		}

		return nil
	})
}

func (test *bpnFixture) checkAllNodesReadyAndBootIDsMatch(ctx context.Context) error {
	var failures []error
	for index := range test.nodes {
		if err := test.checkNodeReadyAndBootIDsMatch(ctx, index); err != nil {
			failures = append(failures, fmt.Errorf("node %s: %w", test.nodes[index].initial.Name, err))
		}
	}

	return errors.Join(failures...)
}

func (test *bpnFixture) checkNodeReadyAndBootIDsMatch(ctx context.Context, index int) error {
	node := test.nodes[index]
	current, err := test.getNodeForCleanup(ctx, index)
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
}

func (test *bpnFixture) removeWorkloadFinalizerAndWaitForPodDeletion() error {
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

func (test *bpnFixture) deleteFixtures(ctx context.Context, owners map[types.UID]bool) error {
	objects := []*unstructured.Unstructured{test.nhc, test.template, test.config}
	finished := make(map[types.UID]bool)
	var permanent []error
	err := bpnCleanupPollWithin(ctx, func(call context.Context) error {
		var pending []error
		for _, object := range objects {
			if object == nil || object.GetUID() == "" || finished[object.GetUID()] {
				continue
			}
			deleteErr := test.deleteFixture(call, object)
			if deleteErr == nil || bpnIsPermanent(deleteErr) {
				finished[object.GetUID()] = true
				if deleteErr != nil {
					permanent = append(permanent, deleteErr)
				}
			} else {
				pending = append(pending, deleteErr)
			}
		}
		if len(owners) > 0 {
			if err := test.checkNoOwnedSBRConfigOperands(call, owners); err != nil {
				pending = append(pending, err)
			}
		}

		return errors.Join(pending...)
	})

	return errors.Join(append(permanent, err)...)
}

func (test *bpnFixture) deleteFixture(ctx context.Context, object *unstructured.Unstructured) error {
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
		return bpnPermanentError{fmt.Errorf("%s %s UID changed", object.GetKind(), object.GetName())}
	}
	uid := object.GetUID()
	if err = bpnCleanupAPI(ctx, func(call context.Context) error {
		return APIClient.Delete(call, current, client.Preconditions{UID: &uid})
	}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	err = bpnCleanupAPI(ctx, func(call context.Context) error {
		return APIClient.Get(call, client.ObjectKeyFromObject(object), current)
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.GetUID() != uid {
		return bpnPermanentError{fmt.Errorf("%s %s UID changed during deletion", object.GetKind(), object.GetName())}
	}

	return fmt.Errorf("normal deletion of %s %s remains pending", object.GetKind(), object.GetName())
}

func (test *bpnFixture) recordSBRConfigAndOperandUIDs(owners map[types.UID]bool) error {
	owners[test.config.GetUID()] = true
	_, err := test.listOwnedSBRConfigOperandsAndRecordUIDs(context.Background(), owners)

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

func (test *bpnFixture) checkNoOwnedSBRConfigOperands(ctx context.Context, owners map[types.UID]bool) error {
	remaining, err := test.listOwnedSBRConfigOperandsAndRecordUIDs(ctx, owners)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("configuration-owned operands remain: %s", strings.Join(remaining, ", "))
	}

	return nil
}

func (test *bpnFixture) listOwnedSBRConfigOperandsAndRecordUIDs(
	ctx context.Context, owners map[types.UID]bool,
) ([]string, error) {
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

func (test *bpnFixture) restoreWorkerLabels(ctx context.Context) error {
	finished := make(map[int]bool)
	var permanent []error
	err := bpnCleanupPollWithin(ctx, func(call context.Context) error {
		var pending []error
		for index, node := range test.nodes {
			if finished[index] {
				continue
			}
			labelErr := test.restoreWorkerLabel(call, index)
			if labelErr == nil || bpnIsPermanent(labelErr) {
				finished[index] = true
				if labelErr != nil {
					permanent = append(permanent, fmt.Errorf("node %s label: %w", node.initial.Name, labelErr))
				}
			} else {
				pending = append(pending, fmt.Errorf("node %s label: %w", node.initial.Name, labelErr))
			}
		}

		return errors.Join(pending...)
	})

	return errors.Join(append(permanent, err)...)
}

func (test *bpnFixture) restoreWorkerLabel(ctx context.Context, index int) error {
	node, err := test.getNodeForCleanup(ctx, index)
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
		return bpnPermanentError{fmt.Errorf("worker label has an unowned value %q", current)}
	}
	if originalExists {
		node.Labels[key] = original
	} else {
		delete(node.Labels, key)
	}
	err = bpnCleanupAPI(ctx, func(call context.Context) error {
		_, err := APIClient.CoreV1Interface.Nodes().Update(call, node, metav1.UpdateOptions{})

		return err
	})
	if err != nil {
		return err
	}
	node, err = test.getNodeForCleanup(ctx, index)
	if err != nil {
		return err
	}
	current, currentExists = node.Labels[key]
	if originalExists != currentExists || original != current {
		return fmt.Errorf("worker label restoration awaits confirmation")
	}

	return nil
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
	if err := bpnCoverage(test.crRecorder, from, through); err != nil {
		return err
	}
	for _, node := range test.nodes {
		if err := bpnCoverage(node.recorder, from, through); err != nil {
			return err
		}
	}

	return nil
}

func (test *bpnFixture) checkFinalRestoration(from time.Time) error {
	if err := test.checkRestorationBoots(); err != nil {
		return err
	}
	if err := test.checkNoSBRsOrRestrictionChanges(from); err != nil {
		return err
	}

	return test.checkRestoredRestrictions(context.Background())
}

func (test *bpnFixture) checkRestorationBoots() error {
	if err := test.checkRecordedBootIDsUnchanged(test.baseline, test.peerIndices()...); err != nil {
		return err
	}
	from := test.baseline
	if !test.nodes[0].rebootObservedAt.IsZero() {
		from = test.nodes[0].rebootObservedAt
	}

	return test.checkRecordedBootIDsUnchanged(from, 0)
}

func (test *bpnFixture) stopObserversCloseSSHAndReportEvidence(check func(string, func() error)) {
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
				AddReportEntry("bpn-node-observations "+node.initial.Name, node.recorder.Snapshot(), ReportEntryVisibilityNever)

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
			AddReportEntry("bpn-request-observations", test.crRecorder.Snapshot(), ReportEntryVisibilityNever)

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
	AddReportEntry("bpn-host-boot-samples", samples, ReportEntryVisibilityNever)
}

func (test *bpnFixture) addWorkloadHoldFinalizer() {
	test.workload.Finalizers = append(test.workload.Finalizers, "tests.medik8s.io/hold-workload")
	Expect(test.callAPIWithNodeIdentityChecks(
		func(ctx context.Context) error { return APIClient.Update(ctx, test.workload) },
	)).To(Succeed())
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
