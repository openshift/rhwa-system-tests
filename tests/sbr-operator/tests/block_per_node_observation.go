package tests

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

func bpnStartRecorder[T runtime.Object](config helpers.RecorderConfig[T]) *helpers.ObjectRecorder[T] {
	GinkgoHelper()
	Expect(config.StartTimeout).To(BeNumerically(">", 0), "All initial watches must start within OBSERVER_START")
	config.APICallTimeout = sbrparams.BPNAPICall
	config.RetryInterval = sbrparams.BPNPoll
	config.Logf = GinkgoWriter.Printf
	recorder, err := helpers.StartObjectRecorder(config)
	Expect(err).NotTo(HaveOccurred(), "The recorder must complete its initial list and establish its watch")

	return recorder
}

func (test *bpnFixture) startNodeSBRAndPodRecorders() {
	deadline := time.Now().Add(sbrparams.BPNObserverStart)
	for _, node := range test.nodes {
		nodeAPI := APIClient.CoreV1Interface.Nodes()
		selector := metav1.ListOptions{FieldSelector: "metadata.name=" + node.initial.Name, AllowWatchBookmarks: true}
		node.recorder = bpnStartRecorder(helpers.RecorderConfig[*corev1.Node]{
			Description: "Node/" + node.initial.Name, StartTimeout: time.Until(deadline),
			List: func(ctx context.Context) ([]*corev1.Node, string, error) {
				list, err := nodeAPI.List(ctx, selector)
				if err != nil {
					return nil, "", err
				}
				items := make([]*corev1.Node, 0, len(list.Items))
				for index := range list.Items {
					items = append(items, list.Items[index].DeepCopy())
				}

				return items, list.ResourceVersion, nil
			},
			Watch: func(ctx context.Context, version string) (watch.Interface, error) {
				options := selector
				options.ResourceVersion = version

				return nodeAPI.Watch(ctx, options)
			},
		})
	}

	crAPI := APIClient.Resource(schema.GroupVersionResource{
		Group: sbrparams.CRDGroup, Version: sbrparams.CRDVersion, Resource: "storagebasedremediations",
	})
	test.crRecorder = bpnStartRecorder(helpers.RecorderConfig[*unstructured.Unstructured]{
		Description: "SBR requests", StartTimeout: time.Until(deadline),
		List: func(ctx context.Context) ([]*unstructured.Unstructured, string, error) {
			list, err := crAPI.List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, "", err
			}
			items := make([]*unstructured.Unstructured, 0, len(list.Items))
			for index := range list.Items {
				items = append(items, list.Items[index].DeepCopy())
			}

			return items, list.GetResourceVersion(), nil
		},
		Watch: func(ctx context.Context, version string) (watch.Interface, error) {
			return crAPI.Watch(ctx, metav1.ListOptions{ResourceVersion: version, AllowWatchBookmarks: true})
		},
	})
	// The namespace is not created yet. A cluster-wide watch includes its first Pod event.
	podAPI := APIClient.CoreV1Interface.Pods("")
	test.podRecorder = bpnStartRecorder(helpers.RecorderConfig[*corev1.Pod]{
		Description: "workload and SBR Pods", StartTimeout: time.Until(deadline),
		List: func(ctx context.Context) ([]*corev1.Pod, string, error) {
			list, err := podAPI.List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, "", err
			}
			items := make([]*corev1.Pod, 0, len(list.Items))
			for index := range list.Items {
				items = append(items, list.Items[index].DeepCopy())
			}

			return items, list.ResourceVersion, nil
		},
		Watch: func(ctx context.Context, version string) (watch.Interface, error) {
			return podAPI.Watch(ctx, metav1.ListOptions{ResourceVersion: version, AllowWatchBookmarks: true})
		},
	})
	Expect(time.Now()).To(BeTemporally("<=", deadline), "All initial watches must start within OBSERVER_START")
}

func (test *bpnFixture) startHostBootIDSampling() {
	ctx, cancel := context.WithCancel(test.ctx)
	test.samplingCancel = cancel
	test.samplingDone = make(chan struct{})
	go func() {
		defer close(test.samplingDone)
		ticker := time.NewTicker(sbrparams.BPNPoll)
		defer ticker.Stop()
		for {
			var pending sync.WaitGroup
			for _, node := range test.nodes {
				pending.Add(1)
				go func() {
					defer pending.Done()
					id, err := node.host.BootID(ctx, sbrparams.BPNHostCall)
					sample := bpnBootSample{At: time.Now(), Node: node.initial.Name, ID: id}
					if err != nil {
						sample.Error = err.Error()
					}
					test.mu.Lock()
					test.samples = append(test.samples, sample)
					test.mu.Unlock()
				}()
			}
			pending.Wait()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (test *bpnFixture) waitForHealthyStorage(indices ...int) {
	const status = corev1.ConditionFalse
	if len(indices) == 0 {
		indices = test.workerIndices()
	}
	reason := "Selected Nodes must report storage health " + string(status)
	test.waitForAPICheck(sbrparams.BPNHealthChange, reason, func(ctx context.Context) error {
		for _, index := range indices {
			node, err := test.getNode(ctx, index)
			if err != nil {
				return err
			}
			if bpnStorageUnhealthyStatus(node) != status {
				return fmt.Errorf("node %s storage health is %s, expected %s", node.Name, bpnStorageUnhealthyStatus(node), status)
			}
			observations := test.nodes[index].recorder.Snapshot().Observations
			if len(observations) == 0 || bpnStorageUnhealthyStatus(observations[len(observations)-1].Object) != status {
				return fmt.Errorf("node %s watch has not observed storage health %s", node.Name, status)
			}
		}

		return nil
	})
}

func (test *bpnFixture) waitForStorageUnhealthy(indices ...int) {
	test.waitForStorageUnhealthyWhileChecking(nil, indices...)
}

func (test *bpnFixture) waitForStorageUnhealthyWhileChecking(check func() error, indices ...int) {
	GinkgoHelper()
	if len(indices) == 0 {
		indices = test.workerIndices()
	}
	for _, index := range indices {
		node := test.nodes[index]
		fault := test.healthFaults[index]
		firstObserved, err := test.waitUnhealthy(index, check)
		AddReportEntry("bpn-unhealthy-evidence", map[string]interface{}{
			"node": node.initial.Name, "nodeUID": node.initial.UID, "faultDispatchedAt": fault.DispatchedAt,
			"faultActiveAt": fault.ActiveAt, "deadline": fault.ActiveAt.Add(sbrparams.BPNHealthChange),
			"firstObservedAt": firstObserved,
		})
		Expect(err).NotTo(HaveOccurred(), "Node %s needs timely and current unhealthy evidence", node.initial.Name)
	}
}

func (test *bpnFixture) waitUnhealthy(index int, check func() error) (time.Time, error) {
	node := test.nodes[index]
	fault := test.healthFaults[index]
	ctx, cancel := context.WithDeadline(test.ctx, fault.ActiveAt.Add(sbrparams.BPNHealthChange))
	defer cancel()
	var firstObserved time.Time
	var evidenceErr error
	err := wait.PollUntilContextCancel(ctx, sbrparams.BPNPoll, true, func(context.Context) (bool, error) {
		if err := test.checkNodeIdentities(); err != nil {
			return false, err
		}
		if check != nil {
			if err := check(); err != nil {
				return false, err
			}
		}
		firstObserved, evidenceErr = bpnUnhealthyEvidenceFrom(
			node.recorder.Snapshot().Observations, node.initial.UID, fault)

		return evidenceErr == nil, nil
	})
	if err != nil {
		return firstObserved, fmt.Errorf("node %s health deadline: %w: %w", node.initial.Name, err, evidenceErr)
	}
	err = test.callAPIWithNodeIdentityChecks(func(call context.Context) error {
		current, getErr := test.getNode(call, index)
		if getErr != nil {
			return getErr
		}
		if bpnStorageUnhealthyStatus(current) != corev1.ConditionTrue {
			return fmt.Errorf("node %s must still report True after timely unhealthy evidence", current.Name)
		}

		return nil
	})

	return firstObserved, err
}

func bpnUnhealthyEvidenceFrom(samples []helpers.RecorderObservation[*corev1.Node], nodeUID types.UID,
	fault sbrutils.BlockFaultEvidence,
) (time.Time, error) {
	if nodeUID == "" || fault.DispatchedAt.IsZero() || fault.ActiveAt.IsZero() ||
		fault.ActiveAt.Before(fault.DispatchedAt) {
		return time.Time{}, fmt.Errorf("the original Node UID and confirmed fault times are required")
	}

	return bpnRecordedStorageUnhealthyAt(samples, nodeUID, fault.DispatchedAt,
		fault.ActiveAt.Add(sbrparams.BPNHealthChange))
}

func bpnRecordedStorageUnhealthyAt(
	samples []helpers.RecorderObservation[*corev1.Node], nodeUID types.UID, from, deadline time.Time,
) (time.Time, error) {
	var firstObserved time.Time
	for _, sample := range samples {
		if sample.UID != nodeUID || sample.Object == nil || sample.EventType == watch.Deleted ||
			sample.At.Before(from) || bpnStorageUnhealthyStatus(sample.Object) != corev1.ConditionTrue {
			continue
		}
		if firstObserved.IsZero() || sample.At.Before(firstObserved) {
			firstObserved = sample.At
		}
	}
	if firstObserved.IsZero() {
		return firstObserved, fmt.Errorf("node %s has no unhealthy observation after %s", nodeUID, from)
	}
	if firstObserved.After(deadline) {
		return firstObserved, fmt.Errorf("node %s first reported True after its health deadline", nodeUID)
	}

	return firstObserved, nil
}

func (test *bpnFixture) waitForRecordedStorageUnhealthy(from time.Time) time.Time {
	GinkgoHelper()
	node := test.nodes[0]
	deadline := from.Add(sbrparams.BPNHealthChange)
	var observedAt time.Time
	reason := "The target must report SBRStorageUnhealthy=True after the crash within HEALTH_CHANGE"
	test.waitForAPICheck(bpnReplayWait(deadline), reason,
		func(_ context.Context) error {
			var err error
			observedAt, err = bpnRecordedStorageUnhealthyAt(node.recorder.Snapshot().Observations,
				node.initial.UID, from, deadline)

			return err
		})
	AddReportEntry("bpn-crash-unhealthy", map[string]interface{}{
		"node": node.initial.Name, "nodeUID": node.initial.UID, "crashDispatchedAt": from, "unhealthyObservedAt": observedAt,
	})

	return observedAt
}

func bpnStorageUnhealthyStatus(node *corev1.Node) corev1.ConditionStatus {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeConditionType(sbrparams.SBRStorageUnhealthyCondition) {
			return condition.Status
		}
	}

	return ""
}

func (test *bpnFixture) checkNodeIdentities() error {
	for _, node := range test.nodes {
		if node.recorder == nil {
			continue
		}

		for _, sample := range node.recorder.Snapshot().Observations {
			if sample.UID != node.initial.UID || sample.EventType == watch.Deleted {
				return bpnPermanentError{fmt.Errorf("node %s must retain UID %s, observed UID %s event %s",
					node.initial.Name, node.initial.UID, sample.UID, sample.EventType)}
			}
		}
	}

	return nil
}

func (test *bpnFixture) checkRecordedBootIDsUnchanged(from time.Time, indices ...int) error {
	if err := test.checkNodeIdentities(); err != nil {
		return err
	}
	if len(indices) == 0 {
		indices = test.workerIndices()
	}
	test.mu.Lock()
	samples := append([]bpnBootSample(nil), test.samples...)
	test.mu.Unlock()
	for _, index := range indices {
		node := test.nodes[index]
		for _, sample := range samples {
			if sample.Node == node.initial.Name && !sample.At.Before(from) && sample.ID != "" && sample.ID != node.boot {
				return fmt.Errorf("node %s rebooted at %s: %s differs from %s", sample.Node, sample.At, sample.ID, node.boot)
			}
		}
	}

	return nil
}

func (test *bpnFixture) checkRecordedTargetBootChange(from time.Time, originalBoot string) error {
	test.mu.Lock()
	defer test.mu.Unlock()

	return bpnCheckTargetBootChange(test.samples, test.nodes[0], from, originalBoot)
}

func bpnCheckTargetBootChange(samples []bpnBootSample, node *bpnNode, from time.Time, originalBoot string) error {
	for _, sample := range samples {
		if sample.Node != node.initial.Name || sample.At.Before(from) || sample.ID == "" {
			continue
		}
		// An SSH read of the original boot can finish after the first read of the new boot.
		if sample.ID != originalBoot && sample.ID != node.boot {
			return fmt.Errorf("node %s reported an extra boot ID %s at %s", sample.Node, sample.ID, sample.At)
		}
	}

	return nil
}

func (test *bpnFixture) assertCurrentHostBootIDsMatch(indices ...int) {
	GinkgoHelper()
	Expect(test.checkNodeIdentities()).To(Succeed(), "Selected Node identities must remain unchanged")
	if len(indices) == 0 {
		indices = test.workerIndices()
	}
	for _, index := range indices {
		node := test.nodes[index]
		Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
			_, err := test.getNode(ctx, index)

			return err
		})).To(Succeed(), "Node %s must retain its original UID", node.initial.Name)
		current, err := node.host.BootID(test.ctx, sbrparams.BPNHostCall)
		Expect(err).NotTo(HaveOccurred(), "INCONCLUSIVE: Node %s needs a final actual boot ID", node.initial.Name)
		Expect(current).To(Equal(node.boot), "Node %s must retain boot ID %s", node.initial.Name, node.boot)
	}
}

func (test *bpnFixture) checkNoSBRsOrRestrictionChanges(from time.Time, indices ...int) error {
	return test.checkOnlyExpectedSBRWithoutRestrictionChanges(from, "", false, indices...)
}

func (test *bpnFixture) checkOnlyExpectedSBRWithoutRestrictionChanges(
	from time.Time, expectedUID types.UID, allowRejoin bool, indices ...int,
) error {
	if len(indices) == 0 {
		indices = test.workerIndices()
	}
	through := time.Now()
	if err := bpnPermanentCoverage(test.crRecorder.Snapshot(), from, through); err != nil {
		return fmt.Errorf("INCONCLUSIVE: request history: %w", err)
	}
	for _, index := range indices {
		node := test.nodes[index]
		if err := bpnPermanentCoverage(node.recorder.Snapshot(), from, through); err != nil {
			return fmt.Errorf("INCONCLUSIVE: Node %s history: %w", node.initial.Name, err)
		}
	}
	if err := test.checkNodeIdentities(); err != nil {
		return err
	}
	if err := test.checkRecordedRestrictions(from, expectedUID, allowRejoin, indices); err != nil {
		return err
	}
	for _, sample := range test.crRecorder.Snapshot().Observations {
		if sample.At.Before(from) {
			continue
		}
		if expectedUID != "" && sample.UID == expectedUID {
			if err := bpnCheckUnauthorizedRequest(sample.Object); err != nil {
				return err
			}

			continue
		}
		if released, ok := test.released[sample.UID]; ok && released.Before(from) {
			continue
		}
		for _, index := range indices {
			node := test.nodes[index]
			if sample.Object.GetName() == node.initial.Name {
				return fmt.Errorf("unexpected SBR CR %s/%s UID %s",
					sample.Object.GetNamespace(), sample.Object.GetName(), sample.UID)
			}
		}
	}

	return nil
}

func (test *bpnFixture) checkRecordedRestrictions(
	from time.Time, expectedUID types.UID, allowRejoin bool, indices []int,
) error {
	for _, index := range indices {
		node := test.nodes[index]
		for _, sample := range node.recorder.Snapshot().Observations {
			if sample.At.Before(from) || sample.UID != node.initial.UID {
				continue
			}
			if err := bpnCheckObservedRestrictions(node.initial, sample.Object, allowRejoin && index == 0); err != nil {
				return err
			}
			if expectedUID != "" && sample.Object.Annotations[bpnOwnerAnnotation] != "" {
				owner, err := bpnParseRemediationOwnerAnnotation(sample.Object)
				if err != nil {
					return err
				}
				if owner.Execution.RequestUID == expectedUID {
					return fmt.Errorf("node %s selected unauthorized CR UID %s for execution",
						node.initial.Name, expectedUID)
				}
			}
		}
	}

	return nil
}

func bpnCheckObservedRestrictions(initial, current *corev1.Node, allowRejoin bool) error {
	if bpnOutOfService(current) != bpnOutOfService(initial) {
		return fmt.Errorf("node %s changed OutOfService", current.Name)
	}
	keyed, pending := bpnRejoinState(current)
	if allowRejoin && keyed && current.Spec.Unschedulable {
		return nil
	}
	if allowRejoin && !keyed && pending == corev1.ConditionTrue &&
		current.Spec.Unschedulable == initial.Spec.Unschedulable {
		return nil
	}
	if current.Spec.Unschedulable != initial.Spec.Unschedulable {
		return fmt.Errorf("node %s changed its cordon", current.Name)
	}

	return bpnCheckClearedRejoin(current)
}

func bpnOutOfService(node *corev1.Node) string {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node.kubernetes.io/out-of-service" {
			return taint.Value + ":" + string(taint.Effect)
		}
	}

	return ""
}

func (test *bpnFixture) waitForCompleteAPIHistory(from, through time.Time, includePods bool) {
	GinkgoHelper()
	reason := "INCONCLUSIVE: API observations must cover the complete negative interval"
	test.waitForAPICheck(sbrparams.BPNObserverStart, reason, func(_ context.Context) error {
		if err := bpnCoverage(test.crRecorder, from, through); err != nil {
			return err
		}
		for _, node := range test.nodes {
			if err := bpnCoverage(node.recorder, from, through); err != nil {
				return err
			}
		}
		if includePods {
			return bpnCoverage(test.podRecorder, from, through)
		}

		return nil
	})
	Expect(test.checkNodeIdentities()).To(Succeed(), "Selected Node identities must remain unchanged after replay")
}

func (test *bpnFixture) waitForTargetHostBootIDChange(from time.Time) (string, time.Time) {
	GinkgoHelper()
	node := test.nodes[0]
	Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		_, err := test.getNode(ctx, 0)

		return err
	})).To(Succeed(), "The positive reboot check must refer to the original Node UID")
	var current string
	remaining := time.Until(from.Add(sbrparams.BPNReboot))
	Expect(remaining).To(BeNumerically(">", 0), "The Node reboot must finish before its deadline")
	Eventually(func() error {
		if err := test.checkNodeIdentities(); err != nil {
			return bpnRetryResult(err)
		}
		var err error
		current, err = node.host.BootID(test.ctx, sbrparams.BPNHostCall)
		if err != nil {
			return err
		}
		if current == "" || current == node.boot {
			return fmt.Errorf("node %s retains boot ID %s", node.initial.Name, current)
		}
		node.rebootObservedAt = time.Now()
		if node.rebootObservedAt.After(from.Add(sbrparams.BPNReboot)) {
			return fmt.Errorf("changed host boot ID was observed after REBOOT")
		}

		return nil
	}, remaining, sbrparams.BPNPoll).Should(Succeed(), "Node %s must reboot within REBOOT", node.initial.Name)
	Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		_, err := test.getNode(ctx, 0)

		return err
	})).To(Succeed(), "The rebooted Node must retain its original UID")
	test.assertPeerHostBootIDsUnchanged()
	AddReportEntry("bpn-reboot", map[string]interface{}{
		"node": node.initial.Name, "nodeUID": node.initial.UID,
		"before": node.boot, "after": current, "observedAt": node.rebootObservedAt,
	})

	return current, node.rebootObservedAt
}

type bpnOwner struct {
	Version   int       `json:"version"`
	NodeUID   types.UID `json:"nodeUID"`
	Execution struct {
		RequestUID types.UID `json:"requestUID"`
		Phase      string    `json:"phase"`
	} `json:"execution"`
	Budget struct {
		Phase string `json:"phase"`
	} `json:"budget"`
}

func bpnParseRemediationOwnerAnnotation(node *corev1.Node) (bpnOwner, error) {
	var owner bpnOwner
	value := node.Annotations[bpnOwnerAnnotation]
	if value == "" {
		return owner, fmt.Errorf("node %s must expose %s", node.Name, bpnOwnerAnnotation)
	}
	if err := json.Unmarshal([]byte(value), &owner); err != nil {
		return owner, err
	}
	if owner.Version != 1 || owner.NodeUID != node.UID {
		return owner, fmt.Errorf("node %s has invalid public owner identity", node.Name)
	}

	return owner, nil
}

func (test *bpnFixture) requireKnownHistoryAndObserveRecovery() {
	GinkgoHelper()
	var owner bpnOwner
	prior, priorErr := bpnParseRemediationOwnerAnnotation(test.nodes[0].initial)
	reason := "The feature must expose known public autonomous history"
	test.waitForAPICheck(sbrparams.BPNBaseline, reason, func(ctx context.Context) error {
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		owner, err = bpnParseRemediationOwnerAnnotation(node)
		if err != nil {
			return err
		}
		if owner.Budget.Phase == "Known" ||
			(owner.Budget.Phase == "Unknown" && priorErr == nil && prior.Budget.Phase == "Unknown") {
			return nil
		}

		return fmt.Errorf("the feature must initialize known autonomous history, observed %q", owner.Budget.Phase)
	})
	if owner.Budget.Phase == "Unknown" {
		if priorErr == nil && prior.Budget.Phase == "Unknown" {
			Skip("BLOCKED: pre-existing autonomous history is Unknown. Preserve its records")
		}
	}
	Expect(owner.Budget.Phase).To(Equal("Known"), "The feature must initialize known autonomous history")
	duration := test.recoveryPeriod + sbrparams.BPNRearmSettle
	if test.baseline.IsZero() {
		test.observeAndRecordHealthyBaseline(duration)
	} else {
		test.observeHealthyWithoutRequests(duration)
	}
}

func (test *bpnFixture) createUnauthorizedTargetSBRCR(copyNHCOwner bool) (*unstructured.Unstructured, time.Time) {
	GinkgoHelper()
	request := buildSBRUnstructured("StorageBasedRemediation", test.nodes[0].initial.Name, map[string]interface{}{})
	if copyNHCOwner {
		controller := false
		request.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: test.nhc.GetAPIVersion(), Kind: test.nhc.GetKind(), Name: test.nhc.GetName(),
			UID: test.nhc.GetUID(), Controller: &controller,
		}})
	}
	from, err := test.createAndTrackSBRCR(request)
	if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
		AddReportEntry("bpn-unauthorized-request-denied", err.Error())

		return nil, from
	}
	Expect(err).NotTo(HaveOccurred(), "The API must return an explicit rejection or the stored unauthorized CR")

	return request, from
}

func (test *bpnFixture) observeUnauthorizedRequestWithoutRemediation(
	request *unstructured.Unstructured, from time.Time,
) {
	GinkgoHelper()
	var allowedUID types.UID
	if request != nil {
		allowedUID = request.GetUID()
	}
	test.observeInterval(sbrparams.BPNNoReboot, from, func() error {
		if err := test.checkRecordedBootIDsUnchanged(from); err != nil {
			return err
		}

		return test.checkOnlyExpectedSBRWithoutRestrictionChanges(from, allowedUID, false)
	})
}

func bpnCheckUnauthorizedRequest(request *unstructured.Unstructured) error {
	phase, _, _ := unstructured.NestedString(request.Object, "status", "executionPhase")
	_, offered, _ := unstructured.NestedFieldNoCopy(request.Object, "status", "offeredAt")
	_, deadline, _ := unstructured.NestedFieldNoCopy(request.Object, "status", "rebootDeadline")
	applied, _, _ := unstructured.NestedBool(request.Object, "status", "cleanup", "outOfServiceApplied")
	if phase != "" || offered || deadline || applied || bpnCondition(request, "FencingInProgress", "True") ||
		bpnCondition(request, "FencingSucceeded", "True") || bpnCondition(request, "Succeeded", "True") {
		return fmt.Errorf("unauthorized CR UID %s started or completed remediation", request.GetUID())
	}

	return nil
}

func (test *bpnFixture) waitForNHCOwnedSBRCR() (*unstructured.Unstructured, time.Time) {
	GinkgoHelper()
	var request *unstructured.Unstructured
	test.waitForAPICheck(sbrparams.BPNRequestCreate, "NHC must create the request", func(ctx context.Context) error {
		current, err := bpnResource("storagebasedremediations").Get(ctx, test.nodes[0].initial.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, owner := range current.GetOwnerReferences() {
			if owner.UID == test.nhc.GetUID() {
				request = current

				return nil
			}
		}

		return fmt.Errorf("the target request must belong to the test NHC")
	})
	test.trackNHCOwnedSBRCR(request)
	from := request.GetCreationTimestamp().Time
	if from.IsZero() {
		from = time.Now()
	}

	return request, from
}

func (test *bpnFixture) trackNHCOwnedSBRCR(request *unstructured.Unstructured) {
	test.requests = append(test.requests, request)
	if test.nhcRequests == nil {
		test.nhcRequests = map[types.UID]bool{}
	}
	test.nhcRequests[request.GetUID()] = true
}

func (test *bpnFixture) waitForRecordedNHCOwnedSBRCR(from, deadline time.Time) (*unstructured.Unstructured, time.Time) {
	GinkgoHelper()
	var request *unstructured.Unstructured
	var observedAt time.Time
	node := test.nodes[0].initial
	reason := "NHC must create a target SBR CR within REQUEST_CREATE after storage becomes unhealthy"
	test.waitForAPICheck(bpnReplayWait(deadline), reason,
		func(_ context.Context) error {
			var err error
			request, observedAt, err = bpnRecordedNHCOwnedSBRCR(test.crRecorder.Snapshot().Observations,
				node, test.nhc.GetUID(), from, deadline)

			return err
		})
	test.trackNHCOwnedSBRCR(request)
	AddReportEntry("bpn-crash-request", map[string]interface{}{
		"node": node.Name, "requestUID": request.GetUID(), "requestObservedAt": observedAt,
	})

	return request, observedAt
}

func bpnRecordedNHCOwnedSBRCR(
	samples []helpers.RecorderObservation[*unstructured.Unstructured], node *corev1.Node,
	nhcUID types.UID, from, deadline time.Time,
) (*unstructured.Unstructured, time.Time, error) {
	for _, sample := range samples {
		request := sample.Object
		if request == nil || sample.At.Before(from) || sample.At.After(deadline) || sample.EventType == watch.Deleted ||
			request.GetName() != node.Name || request.GetNamespace() != medik8sparams.OperatorNs || request.GetUID() == "" {
			continue
		}
		for _, owner := range request.GetOwnerReferences() {
			if owner.UID == nhcUID {
				if err := bpnRequestIdentity(request, request, node); err != nil {
					return nil, time.Time{}, err
				}

				return request, sample.At, nil
			}
		}
	}

	return nil, time.Time{}, fmt.Errorf("no timely SBR CR for node %s with NHC owner UID %s", node.Name, nhcUID)
}

func bpnCheckCrashRequestLifetime(
	samples []helpers.RecorderObservation[*unstructured.Unstructured], node *corev1.Node,
	request *unstructured.Unstructured, baseline, crashAt time.Time,
) error {
	deleted := false
	for _, sample := range samples {
		if sample.At.Before(baseline) || sample.Object.GetName() != request.GetName() {
			continue
		}
		if sample.At.Before(crashAt) {
			return fmt.Errorf("SBR CR for node %s appeared before the crash", request.GetName())
		}
		if err := bpnRequestIdentity(sample.Object, request, node); err != nil {
			return err
		}
		if sample.EventType == watch.Deleted {
			deleted = true
		}
	}
	if !deleted {
		return fmt.Errorf("SBR CR UID %s has no recorded Delete event", request.GetUID())
	}

	return nil
}

const bpnRequestIntentAnnotation = "tests.medik8s.io/block-per-node-request"

func (test *bpnFixture) createAndTrackSBRCR(object *unstructured.Unstructured) (time.Time, error) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[bpnRequestIntentAnnotation] = rand.Text()
	object.SetAnnotations(annotations)
	intent := object.DeepCopy()
	test.requests = append(test.requests, intent)
	dispatched, rejected := false, false
	dispatchedAt := time.Now()
	err := test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		dispatched = true
		createErr := APIClient.Create(ctx, object)
		rejected = bpnCreationRejected(createErr)
		if createErr == nil && object.GetUID() == "" {
			return fmt.Errorf("request creation returned no UID")
		}
		if createErr == nil {
			intent.SetUID(object.GetUID())
		}

		return createErr
	})
	if !dispatched || rejected {
		test.requests = test.requests[:len(test.requests)-1]
	}

	return dispatchedAt, err
}

func bpnCreationRejected(err error) bool {
	return apierrors.IsAlreadyExists(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) ||
		apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsMethodNotSupported(err)
}

func (test *bpnFixture) getAndCheckSBRCR(
	ctx context.Context, expected *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	current, err := bpnResource("storagebasedremediations").Get(ctx, expected.GetName(), metav1.GetOptions{})
	if err == nil {
		err = bpnRequestIdentity(current, expected, test.nodes[0].initial)
	}

	return current, err
}

func bpnCondition(object *unstructured.Unstructured, name, status string) bool {
	_, found := bpnConditionReason(object, name, status)

	return found
}

func bpnConditionReason(object *unstructured.Unstructured, name, status string) (string, bool) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if ok && condition["type"] == name && condition["status"] == status {
			reason, _ := condition["reason"].(string)

			return reason, true
		}
	}

	return "", false
}

func bpnCleanupTimes(object *unstructured.Unstructured) (time.Time, time.Time, error) {
	start, _, _ := unstructured.NestedString(object.Object, "status", "cleanup", "startedAt")
	deadline, _, _ := unstructured.NestedString(object.Object, "status", "cleanup", "deadline")
	startedAt, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("CR cleanup.startedAt must exist: %w", err)
	}
	if startedAt.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("CR cleanup.startedAt must not be zero")
	}
	endsAt, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("CR cleanup.deadline must exist: %w", err)
	}
	if !endsAt.Equal(startedAt.Add(sbrparams.BPNCleanupPeriod)) {
		return time.Time{}, time.Time{}, fmt.Errorf("cleanup deadline must equal start plus 300s")
	}

	return startedAt, endsAt, nil
}

func bpnRequestIdentity(current, expected *unstructured.Unstructured, node *corev1.Node) error {
	if current.GetUID() != expected.GetUID() || current.GetName() != expected.GetName() ||
		current.GetNamespace() != expected.GetNamespace() || current.GetName() != node.Name {
		return bpnPermanentError{fmt.Errorf("SBR CR must retain its UID, namespace, and target Node name")}
	}
	nodeUID, found, err := unstructured.NestedString(current.Object, "status", "nodeUID")
	if err != nil {
		return fmt.Errorf("SBR CR status.nodeUID is invalid: %w", err)
	}
	if found && nodeUID != "" && types.UID(nodeUID) != node.UID {
		return bpnPermanentError{fmt.Errorf("SBR CR status.nodeUID %s differs from original Node UID %s", nodeUID, node.UID)}
	}

	return nil
}

func bpnCheckSBRSuccessAndCleanupResult(object *unstructured.Unstructured) error {
	if err := bpnCheckOOSPolicySuccess(object); err != nil {
		return err
	}
	phase, _, err := unstructured.NestedString(object.Object, "status", "cleanup", "phase")
	if err != nil || (phase != "Completed" && phase != "Incomplete" && phase != "Unknown") {
		return fmt.Errorf("terminal CR cleanup.phase must be Completed, Incomplete, or Unknown, observed %q", phase)
	}

	return nil
}

type bpnCleanupEvidence struct {
	startedAt, deadline, publishedAt time.Time
	result                           *unstructured.Unstructured
}

func bpnStoredCleanupTime(object *unstructured.Unstructured, field string, expected time.Time, assigned *bool) error {
	value, found, err := unstructured.NestedString(object.Object, "status", "cleanup", field)
	if err != nil {
		return fmt.Errorf("cleanup.%s is invalid: %w", field, err)
	}
	if !found || value == "" {
		if *assigned {
			return fmt.Errorf("stored cleanup.%s disappeared", field)
		}

		return nil
	}
	observed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !observed.Equal(expected) {
		return fmt.Errorf("stored cleanup.%s changed from %s to %q", field, expected, value)
	}
	*assigned = true

	return nil
}

func bpnCleanupHistory(
	samples []helpers.RecorderObservation[*unstructured.Unstructured], request *unstructured.Unstructured,
	node *corev1.Node, evidence bpnCleanupEvidence,
) error {
	var startAssigned, deadlineAssigned bool
	for _, sample := range samples {
		if sample.UID != request.GetUID() {
			continue
		}
		if err := bpnRequestIdentity(sample.Object, request, node); err != nil {
			return err
		}
		if sample.EventType == watch.Deleted {
			if sample.At.Before(evidence.publishedAt) {
				return fmt.Errorf("SBR CR disappeared before policy success")
			}

			continue
		}
		if err := bpnStoredCleanupTime(sample.Object, "startedAt", evidence.startedAt, &startAssigned); err != nil {
			return err
		}
		if err := bpnStoredCleanupTime(sample.Object, "deadline", evidence.deadline, &deadlineAssigned); err != nil {
			return err
		}
	}

	return nil
}

func bpnRetainedCleanupResult(object *unstructured.Unstructured, observedAt, deadline time.Time) error {
	phase, _, _ := unstructured.NestedString(object.Object, "status", "cleanup", "phase")
	if phase == "Completed" {
		return fmt.Errorf("cleanup must remain Incomplete or Unknown while the original Pod remains")
	}
	if (phase == "Incomplete" || phase == "Unknown") && observedAt.Before(deadline) {
		return fmt.Errorf("the cleanup observation ended before its deadline with a retained Pod")
	}

	return nil
}

func bpnReplayWait(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}

	return remaining + sbrparams.BPNObserverStart
}

func (test *bpnFixture) waitForRecordedOutOfServiceTaint(from, deadline time.Time) {
	GinkgoHelper()
	reason := "The target must expose exact OutOfService before the required deadline"
	test.waitForAPICheck(bpnReplayWait(deadline), reason, func(ctx context.Context) error {
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			if sample.UID == test.nodes[0].initial.UID && !sample.At.Before(from) && sample.At.Before(deadline) &&
				bpnOutOfService(sample.Object) == "nodeshutdown:NoExecute" {
				return nil
			}
		}
		node, err := test.getNode(ctx, 0)
		if bpnIsPermanent(err) {
			return err
		}
		if err == nil && time.Now().Before(deadline) && bpnOutOfService(node) == "nodeshutdown:NoExecute" {
			return nil
		}
		if time.Now().Before(deadline) {
			return fmt.Errorf("the exact OutOfService taint has not been observed yet")
		}
		if err := bpnCoverage(test.nodes[0].recorder, from, deadline); err != nil {
			return fmt.Errorf("INCONCLUSIVE: no timely OutOfService evidence and incomplete Node history: %w", err)
		}

		return fmt.Errorf("the exact OutOfService taint was not observed before the required deadline")
	})
}

func bpnSelectedPod(pod *corev1.Pod, configName, nodeName string) bool {
	return pod.Namespace == medik8sparams.OperatorNs &&
		pod.Spec.NodeName == nodeName && pod.Labels["sbrconfig"] == configName
}

func (test *bpnFixture) runHost(command string) string {
	GinkgoHelper()
	output, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, command)
	Expect(err).NotTo(HaveOccurred(), "Host command on Node %s must complete: %s", test.nodes[0].initial.Name, command)

	return strings.TrimSpace(output)
}

func (test *bpnFixture) observeNoRebootWithoutRequests() {
	from := time.Now()
	negativeFrom := test.negativeObservationStart(from)
	test.observeInterval(sbrparams.BPNNoReboot, negativeFrom, func() error {
		return test.checkBootsWithoutRequests(from, negativeFrom)
	})
}

func (test *bpnFixture) observeHealthyWithoutRequests(duration time.Duration) {
	from := time.Now()
	negativeFrom := test.negativeObservationStart(from)
	test.observeInterval(duration, negativeFrom, func() error {
		if err := test.checkBootsWithoutRequests(from, negativeFrom); err != nil {
			return err
		}

		return test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, from, time.Now())
	})
}

func (test *bpnFixture) negativeObservationStart(from time.Time) time.Time {
	if !test.quietSince.IsZero() {
		return test.quietSince
	}

	return from
}

func (test *bpnFixture) checkHealthyBoots(from time.Time) error {
	if err := test.checkRecordedBootIDsUnchanged(from); err != nil {
		return err
	}

	return test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, from, time.Now())
}

func (test *bpnFixture) checkBootsWithoutRequests(from, negativeFrom time.Time) error {
	if err := test.checkRecordedBootIDsUnchanged(from); err != nil {
		return err
	}

	return test.checkNoSBRsOrRestrictionChanges(negativeFrom)
}

func (test *bpnFixture) observeInterval(duration time.Duration, replayFrom time.Time, check func() error) {
	GinkgoHelper()
	Consistently(check, duration, sbrparams.BPNPoll).Should(Succeed())
	through := time.Now()
	test.assertCurrentHostBootIDsMatch()
	if !replayFrom.IsZero() {
		test.waitForCompleteAPIHistory(replayFrom, through, false)
	}
	Expect(check()).To(Succeed(), "Replayed observations must satisfy the complete negative interval")
}

func bpnRetainedCleanupHistory(samples []helpers.RecorderObservation[*unstructured.Unstructured],
	request *unstructured.Unstructured, deadline time.Time,
) error {
	for _, sample := range samples {
		if sample.UID == request.GetUID() {
			if err := bpnRetainedCleanupResult(sample.Object, sample.At, deadline); err != nil {
				return err
			}
		}
	}

	return nil
}

func (test *bpnFixture) assertPeerHostBootIDsUnchanged() {
	GinkgoHelper()
	peers := test.peerIndices()
	Expect(test.checkRecordedBootIDsUnchanged(test.baseline, peers...)).To(Succeed(),
		"Peer Nodes must retain boot IDs throughout the case")
	test.assertCurrentHostBootIDsMatch(peers...)
}

// Permanent evidence failures cannot become valid through another poll.
type bpnPermanentError struct{ error }

func (err bpnPermanentError) Unwrap() error {
	return err.error
}

func bpnIsPermanent(err error) bool {
	var permanent bpnPermanentError

	return errors.As(err, &permanent)
}

func bpnRetryResult(err error) error {
	if bpnIsPermanent(err) {
		return StopTrying("Required evidence cannot recover").Wrap(err)
	}

	return err
}

func bpnCoverage[T runtime.Object](recorder *helpers.ObjectRecorder[T], from, through time.Time) error {
	if err := bpnPermanentCoverage(recorder.Snapshot(), from, through); err != nil {
		return err
	}

	return recorder.Coverage(from, through)
}

func bpnPermanentCoverage[T runtime.Object](snapshot helpers.RecorderSnapshot[T], from, through time.Time) error {
	for _, gap := range snapshot.Gaps {
		if !gap.StartedAt.After(through) && !gap.EndedAt.Before(from) {
			return bpnPermanentError{fmt.Errorf("unavailable Watch history: %s", gap.Reason)}
		}
	}

	return nil
}

func (test *bpnFixture) observeStorageFailureWithoutRemediation(indices ...int) {
	GinkgoHelper()
	deadline, err := bpnStorageObservationDeadline(test.healthFaults, indices...)
	Expect(err).NotTo(HaveOccurred(), "The negative interval needs confirmed storage faults")
	from := deadline.Add(-sbrparams.BPNNoReboot)
	negativeFrom := test.negativeObservationStart(from)
	check := func() error { return test.checkBootsWithoutRequests(from, negativeFrom) }
	test.waitForStorageUnhealthyWhileChecking(check, indices...)
	remaining := time.Until(deadline)
	if remaining > 0 {
		test.observeInterval(remaining, negativeFrom, check)

		return
	}
	test.assertCurrentHostBootIDsMatch()
	test.waitForCompleteAPIHistory(negativeFrom, time.Now(), false)
	Expect(check()).To(Succeed(), "The complete fault interval must contain no reboot or request")
}

func bpnStorageObservationDeadline(faults map[int]sbrutils.BlockFaultEvidence, indices ...int) (time.Time, error) {
	var last time.Time
	for _, index := range indices {
		fault := faults[index]
		if fault.DispatchedAt.IsZero() || fault.ActiveAt.IsZero() || fault.ActiveAt.Before(fault.DispatchedAt) {
			return time.Time{}, fmt.Errorf("node %d needs confirmed fault times", index)
		}
		if fault.ActiveAt.After(last) {
			last = fault.ActiveAt
		}
	}
	if last.IsZero() {
		return time.Time{}, fmt.Errorf("the interval needs at least one confirmed fault")
	}

	return last.Add(sbrparams.BPNNoReboot), nil
}
