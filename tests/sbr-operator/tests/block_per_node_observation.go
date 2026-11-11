package tests

import (
	"context"
	"crypto/rand"
	"encoding/json"
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

func (test *bpnCase) startObservation() {
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

func (test *bpnCase) startBootSampling() {
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

func (test *bpnCase) requireHealth(status corev1.ConditionStatus, indices ...int) {
	if len(indices) == 0 {
		indices = []int{0, 1, 2}
	}
	reason := "Selected Nodes must report storage health " + string(status)
	test.eventually(sbrparams.BPNHealthChange, reason, func(ctx context.Context) error {
		for _, index := range indices {
			node, err := test.getNode(ctx, index)
			if err != nil {
				return err
			}
			if bpnHealth(node) != status {
				return fmt.Errorf("node %s storage health is %s, expected %s", node.Name, bpnHealth(node), status)
			}
			observations := test.nodes[index].recorder.Snapshot().Observations
			if len(observations) == 0 || bpnHealth(observations[len(observations)-1].Object) != status {
				return fmt.Errorf("node %s watch has not observed storage health %s", node.Name, status)
			}
		}

		return nil
	})
}

func (test *bpnCase) requireUnhealthy(indices ...int) {
	GinkgoHelper()
	if len(indices) == 0 {
		indices = []int{0, 1, 2}
	}
	for _, index := range indices {
		node := test.nodes[index]
		fault := test.healthFaults[index]
		firstObserved, err := test.waitUnhealthy(index)
		AddReportEntry("bpn-unhealthy-evidence", map[string]interface{}{
			"node": node.initial.Name, "nodeUID": node.initial.UID, "faultDispatchedAt": fault.DispatchedAt,
			"faultActiveAt": fault.ActiveAt, "deadline": fault.ActiveAt.Add(sbrparams.BPNHealthChange),
			"firstObservedAt": firstObserved,
		})
		Expect(err).NotTo(HaveOccurred(), "Node %s needs timely and current unhealthy evidence", node.initial.Name)
	}
}

func (test *bpnCase) waitUnhealthy(index int) (time.Time, error) {
	node := test.nodes[index]
	fault := test.healthFaults[index]
	ctx, cancel := context.WithDeadline(test.ctx, fault.ActiveAt.Add(sbrparams.BPNHealthChange))
	defer cancel()
	var firstObserved time.Time
	var evidenceErr error
	err := wait.PollUntilContextCancel(ctx, sbrparams.BPNPoll, true, func(context.Context) (bool, error) {
		firstObserved, evidenceErr = bpnUnhealthyEvidenceFrom(
			node.recorder.Snapshot().Observations, node.initial.UID, fault)

		return evidenceErr == nil, nil
	})
	if err != nil {
		return firstObserved, fmt.Errorf("node %s health deadline: %w: %w", node.initial.Name, err, evidenceErr)
	}
	err = test.api(func(call context.Context) error {
		current, getErr := test.getNode(call, index)
		if getErr != nil {
			return getErr
		}
		if bpnHealth(current) != corev1.ConditionTrue {
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
	var firstObserved time.Time
	for _, sample := range samples {
		if sample.UID != nodeUID || sample.Object == nil || sample.EventType == watch.Deleted ||
			sample.At.Before(fault.DispatchedAt) || bpnHealth(sample.Object) != corev1.ConditionTrue {
			continue
		}
		if firstObserved.IsZero() || sample.At.Before(firstObserved) {
			firstObserved = sample.At
		}
	}
	if firstObserved.IsZero() {
		return firstObserved, fmt.Errorf("node %s has no unhealthy observation for this fault", nodeUID)
	}
	if firstObserved.After(fault.ActiveAt.Add(sbrparams.BPNHealthChange)) {
		return firstObserved, fmt.Errorf("node %s first reported True after its health deadline", nodeUID)
	}

	return firstObserved, nil
}

func bpnHealth(node *corev1.Node) corev1.ConditionStatus {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeConditionType(sbrparams.SBRStorageUnhealthyCondition) {
			return condition.Status
		}
	}

	return ""
}

func (test *bpnCase) checkNodeIdentities() error {
	for _, node := range test.nodes {
		if node.recorder == nil {
			continue
		}

		for _, sample := range node.recorder.Snapshot().Observations {
			if sample.UID != node.initial.UID || sample.EventType == watch.Deleted {
				return fmt.Errorf("node %s must retain UID %s, observed UID %s event %s",
					node.initial.Name, node.initial.UID, sample.UID, sample.EventType)
			}
		}
	}

	return nil
}

func (test *bpnCase) checkBoots(from time.Time, indices ...int) error {
	if err := test.checkNodeIdentities(); err != nil {
		return err
	}
	if len(indices) == 0 {
		indices = []int{0, 1, 2}
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

func (test *bpnCase) finalBoots(indices ...int) {
	GinkgoHelper()
	Expect(test.checkNodeIdentities()).To(Succeed(), "Selected Node identities must remain unchanged")
	if len(indices) == 0 {
		indices = []int{0, 1, 2}
	}
	for _, index := range indices {
		node := test.nodes[index]
		Expect(test.api(func(ctx context.Context) error {
			_, err := test.getNode(ctx, index)

			return err
		})).To(Succeed(), "Node %s must retain its original UID", node.initial.Name)
		current, err := node.host.BootID(test.ctx, sbrparams.BPNHostCall)
		Expect(err).NotTo(HaveOccurred(), "INCONCLUSIVE: Node %s needs a final actual boot ID", node.initial.Name)
		Expect(current).To(Equal(node.boot), "Node %s must retain boot ID %s", node.initial.Name, node.boot)
	}
}

func (test *bpnCase) noRestrictions(from time.Time) error {
	if err := test.checkNodeIdentities(); err != nil {
		return err
	}
	for _, node := range test.nodes {
		for _, sample := range node.recorder.Snapshot().Observations {
			if sample.At.Before(from) || sample.UID != node.initial.UID {
				continue
			}
			if sample.Object.Spec.Unschedulable != node.initial.Spec.Unschedulable {
				return fmt.Errorf("node %s gained a cordon", node.initial.Name)
			}
			if bpnOutOfService(sample.Object) != bpnOutOfService(node.initial) {
				return fmt.Errorf("node %s changed OutOfService", node.initial.Name)
			}
		}
	}
	for _, sample := range test.crRecorder.Snapshot().Observations {
		if sample.At.Before(from) {
			continue
		}
		if released, ok := test.released[sample.UID]; ok && released.Before(from) {
			continue
		}
		for _, node := range test.nodes {
			if sample.Object.GetName() == node.initial.Name {
				return fmt.Errorf("unexpected SBR CR %s/%s UID %s",
					sample.Object.GetNamespace(), sample.Object.GetName(), sample.UID)
			}
		}
	}

	return nil
}

func bpnOutOfService(node *corev1.Node) string {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node.kubernetes.io/out-of-service" {
			return taint.Value + ":" + string(taint.Effect)
		}
	}

	return ""
}

func (test *bpnCase) coverage(from, through time.Time, includePods bool) {
	GinkgoHelper()
	reason := "INCONCLUSIVE: API observations must cover the complete negative interval"
	test.eventually(sbrparams.BPNObserverStart, reason, func(_ context.Context) error {
		if err := test.crRecorder.Coverage(from, through); err != nil {
			return err
		}
		for _, node := range test.nodes {
			if err := node.recorder.Coverage(from, through); err != nil {
				return err
			}
		}
		if includePods {
			return test.podRecorder.Coverage(from, through)
		}

		return nil
	})
	Expect(test.checkNodeIdentities()).To(Succeed(), "Selected Node identities must remain unchanged after replay")
}

func (test *bpnCase) observe(duration time.Duration, healthy, noRequest bool) {
	GinkgoHelper()
	from := time.Now()
	negativeFrom := from
	if noRequest && !test.quietSince.IsZero() {
		negativeFrom = test.quietSince
	}
	check := func() error { return test.checkObservation(from, negativeFrom, healthy, noRequest) }
	Consistently(check, duration, sbrparams.BPNPoll).Should(Succeed())
	through := time.Now()
	test.finalBoots()
	if noRequest || healthy {
		test.coverage(negativeFrom, through, false)
	}
	Expect(check()).To(Succeed(), "Replayed observations must satisfy the complete negative interval")
}

func (test *bpnCase) checkObservation(from, negativeFrom time.Time, healthy, noRequest bool) error {
	if err := test.checkBoots(from); err != nil {
		return err
	}
	if noRequest {
		if err := test.noRestrictions(negativeFrom); err != nil {
			return err
		}
	}
	if healthy {
		return test.healthHistory(corev1.ConditionFalse, from, time.Now(), 0, 1, 2)
	}

	return nil
}

func (test *bpnCase) waitReboot(from time.Time) string {
	GinkgoHelper()
	node := test.nodes[0]
	Expect(test.api(func(ctx context.Context) error {
		_, err := test.getNode(ctx, 0)

		return err
	})).To(Succeed(), "The positive reboot check must refer to the original Node UID")
	var current string
	remaining := time.Until(from.Add(sbrparams.BPNReboot))
	Expect(remaining).To(BeNumerically(">", 0), "The Node reboot must finish before its deadline")
	Eventually(func() error {
		var err error
		current, err = node.host.BootID(test.ctx, sbrparams.BPNHostCall)
		if err != nil {
			return err
		}
		if current == "" || current == node.boot {
			return fmt.Errorf("node %s retains boot ID %s", node.initial.Name, current)
		}
		test.lastReboot = time.Now()
		if test.lastReboot.After(from.Add(sbrparams.BPNReboot)) {
			return fmt.Errorf("changed host boot ID was observed after REBOOT")
		}

		return nil
	}, remaining, sbrparams.BPNPoll).Should(Succeed(), "Node %s must reboot within REBOOT", node.initial.Name)
	Expect(test.api(func(ctx context.Context) error {
		_, err := test.getNode(ctx, 0)

		return err
	})).To(Succeed(), "The rebooted Node must retain its original UID")
	Expect(test.checkBoots(test.baseline, 1, 2)).To(Succeed(), "Peer Nodes must retain boot IDs throughout the case")
	test.finalBoots(1, 2)
	AddReportEntry("bpn-reboot", map[string]interface{}{
		"node": node.initial.Name, "nodeUID": node.initial.UID,
		"before": node.boot, "after": current, "observedAt": test.lastReboot,
	})

	return current
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

func bpnReadOwner(node *corev1.Node) (bpnOwner, error) {
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

func (test *bpnCase) autonomousPreparation() {
	GinkgoHelper()
	var owner bpnOwner
	Expect(test.api(func(ctx context.Context) error {
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		owner, err = bpnReadOwner(node)

		return err
	})).To(Succeed(), "The feature must expose its public autonomous history")
	if owner.Budget.Phase == "Unknown" {
		prior, err := bpnReadOwner(test.nodes[0].initial)
		if err == nil && prior.Budget.Phase == "Unknown" {
			Skip("BLOCKED: pre-existing autonomous history is Unknown. Preserve its records")
		}
	}
	Expect(owner.Budget.Phase).To(Equal("Known"), "The feature must initialize known autonomous history")
	test.observe(test.profile+sbrparams.BPNRearmSettle, true, true)
}

func (test *bpnCase) request() *unstructured.Unstructured {
	GinkgoHelper()
	object := buildSBRUnstructured("StorageBasedRemediation", test.nodes[0].initial.Name, map[string]interface{}{})
	currentBoot, err := test.nodes[0].host.BootID(test.ctx, sbrparams.BPNHostCall)
	Expect(err).NotTo(HaveOccurred(), "The target must expose its actual boot ID before request creation")
	Expect(currentBoot).To(Equal(test.nodes[0].boot), "The target must not reboot before its request is created")
	Expect(test.createRequest(object)).To(Succeed(), "A valid external request must be accepted")

	return object
}

const bpnRequestIntentAnnotation = "tests.medik8s.io/block-per-node-request"

func (test *bpnCase) createRequest(object *unstructured.Unstructured) error {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[bpnRequestIntentAnnotation] = rand.Text()
	object.SetAnnotations(annotations)
	intent := object.DeepCopy()
	test.requests = append(test.requests, intent)
	dispatched, rejected := false, false
	test.trigger = time.Now()
	err := test.api(func(ctx context.Context) error {
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

	return err
}

func bpnCreationRejected(err error) bool {
	return apierrors.IsAlreadyExists(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) ||
		apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsMethodNotSupported(err)
}

func (test *bpnCase) requestCurrent(
	ctx context.Context, expected *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	current, err := bpnResource("storagebasedremediations").Get(ctx, expected.GetName(), metav1.GetOptions{})
	if err == nil {
		err = bpnRequestIdentity(current, expected, test.nodes[0].initial)
	}

	return current, err
}

func bpnCondition(object *unstructured.Unstructured, name, status string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if ok && condition["type"] == name && condition["status"] == status {
			return true
		}
	}

	return false
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
		return fmt.Errorf("SBR CR must retain its UID, namespace, and target Node name")
	}
	nodeUID, found, err := unstructured.NestedString(current.Object, "status", "nodeUID")
	if err != nil {
		return fmt.Errorf("SBR CR status.nodeUID is invalid: %w", err)
	}
	if found && nodeUID != "" && types.UID(nodeUID) != node.UID {
		return fmt.Errorf("SBR CR status.nodeUID %s differs from original Node UID %s", nodeUID, node.UID)
	}

	return nil
}

func bpnTerminalRequestedResult(object *unstructured.Unstructured) error {
	if !bpnCondition(object, "FencingSucceeded", "True") ||
		!bpnCondition(object, "Succeeded", "True") || !bpnCondition(object, "Processing", "False") {
		return fmt.Errorf("CR %s must report FencingSucceeded=True, Succeeded=True, Processing=False", object.GetUID())
	}
	phase, _, err := unstructured.NestedString(object.Object, "status", "cleanup", "phase")
	if err != nil || (phase != "Completed" && phase != "Incomplete" && phase != "Unknown") {
		return fmt.Errorf("terminal CR cleanup.phase must be Completed, Incomplete, or Unknown, observed %q", phase)
	}

	return nil
}

type bpnCleanupEvidence struct {
	startedAt, deadline, publishedAt, terminalAt time.Time
}

func bpnCleanupEvidenceFrom(
	samples []helpers.RecorderObservation[*unstructured.Unstructured], request *unstructured.Unstructured,
	node *corev1.Node, publishedBy time.Time,
) (bpnCleanupEvidence, error) {
	var evidence bpnCleanupEvidence
	for _, sample := range samples {
		if sample.UID != request.GetUID() {
			continue
		}
		if err := bpnRequestIdentity(sample.Object, request, node); err != nil {
			return evidence, err
		}
		start, deadline, err := bpnCleanupTimes(sample.Object)
		if err != nil {
			continue
		}
		if evidence.publishedAt.IsZero() || sample.At.Before(evidence.publishedAt) {
			evidence.startedAt, evidence.deadline, evidence.publishedAt = start, deadline, sample.At
		}
		if bpnTerminalRequestedResult(sample.Object) == nil &&
			(evidence.terminalAt.IsZero() || sample.At.Before(evidence.terminalAt)) {
			evidence.terminalAt = sample.At
		}
	}
	if evidence.publishedAt.IsZero() || evidence.publishedAt.After(publishedBy) {
		return evidence, fmt.Errorf("cleanup.startedAt and cleanup.deadline must be observed within RELEASE after reboot")
	}
	if evidence.startedAt.After(evidence.publishedAt) {
		return evidence, fmt.Errorf("cleanup.startedAt must not follow its publication")
	}

	return evidence, nil
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
	node *corev1.Node, evidence bpnCleanupEvidence, retained bool,
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
			return fmt.Errorf("SBR CR disappeared before normal request deletion")
		}
		if err := bpnStoredCleanupTime(sample.Object, "startedAt", evidence.startedAt, &startAssigned); err != nil {
			return err
		}
		if err := bpnStoredCleanupTime(sample.Object, "deadline", evidence.deadline, &deadlineAssigned); err != nil {
			return err
		}
		if retained {
			if err := bpnRetainedCleanupResult(sample.Object, sample.At, evidence.deadline); err != nil {
				return err
			}
		}
	}

	return nil
}

func bpnRetainedCleanupResult(object *unstructured.Unstructured, observedAt, deadline time.Time) error {
	phase, _, _ := unstructured.NestedString(object.Object, "status", "cleanup", "phase")
	if phase == "Completed" {
		return fmt.Errorf("cleanup must remain Incomplete or Unknown while the original Pod remains")
	}
	if bpnCondition(object, "Succeeded", "True") && observedAt.Before(deadline) {
		return fmt.Errorf("the request succeeded before the cleanup deadline with a retained Pod")
	}

	return nil
}

func (test *bpnCase) requestEvidence(request *unstructured.Unstructured, publishedBy time.Time,
	live *[]helpers.RecorderObservation[*unstructured.Unstructured], ctx context.Context,
) (bpnCleanupEvidence, error) {
	if _, err := test.getNode(ctx, 0); err != nil {
		return bpnCleanupEvidence{}, err
	}
	current, err := test.requestCurrent(ctx, request)
	if err != nil {
		return bpnCleanupEvidence{}, err
	}
	*live = append(*live, helpers.RecorderObservation[*unstructured.Unstructured]{
		At: time.Now(), UID: current.GetUID(), Object: current,
	})
	samples := test.crRecorder.Snapshot().Observations
	samples = append(samples, *live...)

	return bpnCleanupEvidenceFrom(samples, request, test.nodes[0].initial, publishedBy)
}

func bpnReplayWait(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}

	return remaining + sbrparams.BPNObserverStart
}

func (test *bpnCase) requestedResult(request *unstructured.Unstructured, from time.Time) {
	GinkgoHelper()
	currentBoot := test.waitReboot(from)
	publishedBy := test.lastReboot.Add(sbrparams.BPNRelease)
	if test.plan.id == "RHWA-1839-17A" || test.plan.id == "RHWA-1839-17B" {
		test.requireAgentRunning(true)
	}
	var evidence bpnCleanupEvidence
	var live []helpers.RecorderObservation[*unstructured.Unstructured]
	reason := "The request must publish its fixed cleanup times within RELEASE"
	test.eventually(bpnReplayWait(publishedBy), reason, func(ctx context.Context) error {
		var err error
		evidence, err = test.requestEvidence(request, publishedBy, &live, ctx)

		return err
	})
	reason = "The same CR must report terminal success by its cleanup deadline plus PUBLISH"
	test.eventually(bpnReplayWait(evidence.deadline.Add(sbrparams.BPNPublish)), reason, func(ctx context.Context) error {
		var err error
		evidence, err = test.requestEvidence(request, publishedBy, &live, ctx)
		if err != nil {
			return err
		}
		if evidence.terminalAt.IsZero() || evidence.terminalAt.After(evidence.deadline.Add(sbrparams.BPNPublish)) {
			return fmt.Errorf("terminal requested-remediation success was not observed before the publication deadline")
		}

		return nil
	})
	test.requireOutOfService(from, evidence.deadline)
	through := time.Now()
	reason = "INCONCLUSIVE: CR history must cover immutable cleanup times"
	test.eventually(sbrparams.BPNObserverStart, reason, func(_ context.Context) error {
		return test.crRecorder.Coverage(from, through)
	})
	retained := test.plan.id == "RHWA-1839-15A" || test.plan.id == "RHWA-1839-15B"
	Expect(bpnCleanupHistory(test.crRecorder.Snapshot().Observations,
		request, test.nodes[0].initial, evidence, retained)).To(Succeed())
	Expect(test.checkBoots(test.baseline, 1, 2)).To(Succeed(), "Peer Nodes must retain boot IDs throughout the case")
	test.finalBoots(1, 2)
	test.nodes[0].boot = currentBoot
}

func (test *bpnCase) requireOutOfService(from, deadline time.Time) {
	GinkgoHelper()
	reason := "The target must expose exact OutOfService before its cleanup deadline"
	test.eventually(bpnReplayWait(deadline), reason, func(ctx context.Context) error {
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			if sample.UID == test.nodes[0].initial.UID && !sample.At.Before(from) && sample.At.Before(deadline) &&
				bpnOutOfService(sample.Object) == "nodeshutdown:NoExecute" {
				return nil
			}
		}
		node, err := test.getNode(ctx, 0)
		if err == nil && time.Now().Before(deadline) && bpnOutOfService(node) == "nodeshutdown:NoExecute" {
			return nil
		}
		if err := test.nodes[0].recorder.Coverage(from, deadline); err != nil {
			return fmt.Errorf("INCONCLUSIVE: no timely OutOfService evidence and incomplete Node history: %w", err)
		}

		return fmt.Errorf("the exact OutOfService taint was not observed before the cleanup deadline")
	})
}

func (test *bpnCase) acceptance(request *unstructured.Unstructured) time.Time {
	GinkgoHelper()
	var accepted time.Time
	remaining := time.Until(test.trigger.Add(sbrparams.BPNReboot))
	Expect(remaining).To(BeNumerically(">", 0))
	reason := "The matching public Accepted record must appear before REBOOT"
	test.eventually(remaining, reason, func(_ context.Context) error {
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			if sample.At.Before(test.trigger) || sample.UID != test.nodes[0].initial.UID {
				continue
			}
			owner, err := bpnReadOwner(sample.Object)
			if err == nil && owner.Execution.RequestUID == request.GetUID() && owner.Execution.Phase == "Accepted" {
				accepted = sample.At

				return nil
			}
		}

		return fmt.Errorf("matching Accepted record not observed")
	})
	Expect(test.nodes[0].recorder.Coverage(test.trigger, accepted)).To(Succeed(),
		"Row 12 requires complete acceptance evidence")

	return accepted
}

func (test *bpnCase) retainedRequest() types.UID {
	var uid types.UID
	Expect(test.api(func(ctx context.Context) error {
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		owner, err := bpnReadOwner(node)
		uid = owner.Execution.RequestUID

		return err
	})).To(Succeed())

	return uid
}

func bpnSelectedPod(pod *corev1.Pod, configName, nodeName string) bool {
	return pod.Namespace == medik8sparams.OperatorNs &&
		pod.Spec.NodeName == nodeName && pod.Labels["sbrconfig"] == configName
}

func (test *bpnCase) runHost(command string) string {
	GinkgoHelper()
	output, err := test.nodes[0].host.Run(test.ctx, sbrparams.BPNHostCall, command)
	Expect(err).NotTo(HaveOccurred(), "Host command on Node %s must complete: %s", test.nodes[0].initial.Name, command)

	return strings.TrimSpace(output)
}
