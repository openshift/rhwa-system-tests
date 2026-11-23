package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBPNWorkloadDigestRequiresSHA256Hex(t *testing.T) {
	invalid := []string{"sha256:" + strings.Repeat("g", 64), "sha256:" + strings.Repeat("A", 64), "sha256:abc"}
	for _, value := range invalid {
		if bpnValidImageDigest(value) {
			t.Errorf("invalid image digest was accepted: %s", value)
		}
	}
	if !bpnValidImageDigest("sha256:" + strings.Repeat("a", 64)) {
		t.Fatal("valid image digest was rejected")
	}
}

type bpnJournalCleanupHost struct {
	restored bool
}

func (host *bpnJournalCleanupHost) Run(_ context.Context, _ time.Duration, command string) (string, error) {
	if strings.Contains(command, "sudo chattr -i") {
		host.restored = true
	}

	return "-------------- /var/lib/medik8s/state", nil
}

type bpnRefusedCancellationClient struct{ client.Client }

func (bpnRefusedCancellationClient) Get(_ context.Context, _ client.ObjectKey, object client.Object,
	_ ...client.GetOption,
) error {
	object.SetUID("pending-request")

	return nil
}

func (bpnRefusedCancellationClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "storagebasedremediations"}, "worker-a",
		fmt.Errorf("deletion refused"))
}

func TestBPNJournalCleanupKeepsWritesBlockedWhenCancellationFails(t *testing.T) {
	originalClient := APIClient
	APIClient = &clients.Settings{Client: bpnRefusedCancellationClient{}}
	t.Cleanup(func() { APIClient = originalClient })
	host := &bpnJournalCleanupHost{}
	fault, err := sbrutils.NewBlockJournalFault(host, "cleanup-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = fault.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = fault.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := &unstructured.Unstructured{}
	request.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage-based-remediation.medik8s.io",
		Version: "v1alpha1", Kind: "StorageBasedRemediation",
	})
	request.SetName("worker-a")
	request.SetNamespace("operator")
	request.SetUID("pending-request")
	fixture := &bpnFixture{
		requests:      []*unstructured.Unstructured{request},
		journalFaults: []*bpnOwnedJournalFault{{BlockJournalFault: fault, releaseRequestsBeforeRestore: true}},
	}
	var failures []error
	fixture.restoreCleanupFaults(func(_ string, operation func() error) {
		if operationErr := operation(); operationErr != nil {
			failures = append(failures, operationErr)
		}
	})
	if host.restored {
		t.Fatal("cleanup restored journal writes while a request could still execute")
	}
	if len(failures) == 0 {
		t.Fatal("cleanup must report failed cancellation")
	}
}

type bpnCleanupCore struct {
	typedcorev1.CoreV1Interface
	nodes typedcorev1.NodeInterface
}

func (core bpnCleanupCore) Nodes() typedcorev1.NodeInterface {
	return core.nodes
}

type bpnCleanupNodes struct {
	typedcorev1.NodeInterface
	get func(context.Context) (*corev1.Node, error)
}

func (nodes bpnCleanupNodes) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*corev1.Node, error) {
	return nodes.get(ctx)
}

type bpnReleasedRequestClient struct {
	client.Client
	get func(context.Context) error
}

func (requestClient bpnReleasedRequestClient) Get(ctx context.Context, _ client.ObjectKey, _ client.Object,
	_ ...client.GetOption,
) error {
	if err := requestClient.get(ctx); err != nil {
		return err
	}

	return apierrors.NewNotFound(schema.GroupResource{Resource: "storagebasedremediations"}, "worker-a")
}

func bpnNewPendingSBR() *unstructured.Unstructured {
	request := &unstructured.Unstructured{}
	request.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage-based-remediation.medik8s.io",
		Version: "v1alpha1", Kind: "StorageBasedRemediation",
	})
	request.SetName("worker-a")
	request.SetNamespace("operator")
	request.SetUID("pending-request")

	return request
}

func TestBPNRequestReleaseSharesOneDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	expectedDeadline, _ := ctx.Deadline()
	var deletionDeadline, restrictionDeadline time.Time
	requestClient := bpnReleasedRequestClient{get: func(call context.Context) error {
		deletionDeadline, _ = call.Deadline()
		timer := time.NewTimer(30 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-call.Done():
			return call.Err()
		case <-timer.C:
			return nil
		}
	}}
	nodes := bpnCleanupNodes{get: func(call context.Context) (*corev1.Node, error) {
		restrictionDeadline, _ = call.Deadline()
		<-call.Done()

		return nil, call.Err()
	}}
	originalClient := APIClient
	APIClient = &clients.Settings{Client: requestClient, CoreV1Interface: bpnCleanupCore{nodes: nodes}}
	t.Cleanup(func() { APIClient = originalClient })
	fixture := &bpnFixture{nodes: []*bpnNode{{initial: &corev1.Node{}}}}
	if err := fixture.deleteSBRAndWaitForBaselineRestrictionsWithin(
		ctx, bpnNewPendingSBR()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release error = %v, want the shared deadline", err)
	}
	if !deletionDeadline.Equal(expectedDeadline) || !restrictionDeadline.Equal(expectedDeadline) {
		t.Fatalf("deletion deadline %s and restriction deadline %s must equal %s",
			deletionDeadline, restrictionDeadline, expectedDeadline)
	}
}

func TestBPNJournalCleanupRestoresWritesAfterRelease(t *testing.T) {
	host := &bpnJournalCleanupHost{}
	checkedRequest, checkedRestrictions := false, false
	requestClient := bpnReleasedRequestClient{get: func(context.Context) error {
		if host.restored {
			t.Fatal("journal writes returned before request deletion")
		}
		checkedRequest = true

		return nil
	}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "node-a"}}
	nodes := bpnCleanupNodes{get: func(context.Context) (*corev1.Node, error) {
		if !checkedRequest || host.restored {
			t.Fatal("restriction checks must follow deletion and precede journal restoration")
		}
		checkedRestrictions = true

		return node, nil
	}}
	originalClient := APIClient
	APIClient = &clients.Settings{Client: requestClient, CoreV1Interface: bpnCleanupCore{nodes: nodes}}
	t.Cleanup(func() { APIClient = originalClient })
	fault, err := sbrutils.NewBlockJournalFault(host, "cleanup-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = fault.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = fault.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture := &bpnFixture{
		nodes:         []*bpnNode{{initial: node}},
		requests:      []*unstructured.Unstructured{bpnNewPendingSBR()},
		journalFaults: []*bpnOwnedJournalFault{{BlockJournalFault: fault, releaseRequestsBeforeRestore: true}},
	}
	if !fixture.restoreCleanupFaults(func(_ string, operation func() error) {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}) || !host.restored || !checkedRestrictions {
		t.Fatal("cleanup must restore journal writes after deletion and restriction release")
	}
}

func TestBPNRestorationRejectsEarlierPeerReboot(t *testing.T) {
	baseline := time.Now().Add(-time.Minute)
	for _, sampleOffset := range []time.Duration{time.Second, 59 * time.Second} {
		fixture := &bpnFixture{baseline: baseline}
		for index := range 5 {
			fixture.nodes = append(fixture.nodes, &bpnNode{
				initial: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%d", index)}}, boot: "expected",
			})
		}
		fixture.nodes[0].rebootObservedAt = baseline.Add(30 * time.Second)
		fixture.samples = []bpnBootSample{{At: baseline.Add(sampleOffset), Node: "worker-4", ID: "unexpected"}}
		if err := fixture.finalRestoredObservation(); err == nil || !strings.Contains(err.Error(), "worker-4 rebooted") {
			t.Fatalf("peer reboot before final observation must fail: %v", err)
		}
		for _, node := range fixture.nodes {
			if node.boot != "expected" {
				t.Fatal("restoration must preserve expected boot IDs")
			}
		}
	}
}

func TestBPNRestorationAllowsOnlyConfirmedTargetReboot(t *testing.T) {
	baseline := time.Now().Add(-time.Minute)
	fixture := &bpnFixture{baseline: baseline}
	for index := range 3 {
		fixture.nodes = append(fixture.nodes, &bpnNode{
			initial: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%d", index)}}, boot: "expected",
		})
	}
	fixture.nodes[0].boot = "confirmed"
	fixture.nodes[0].rebootObservedAt = baseline.Add(30 * time.Second)
	fixture.samples = []bpnBootSample{
		{At: baseline, Node: "worker-0", ID: "original"},
		{At: fixture.nodes[0].rebootObservedAt, Node: "worker-0", ID: "confirmed"},
	}
	if err := fixture.checkRestorationBoots(); err != nil {
		t.Fatal(err)
	}
	fixture.samples = append(fixture.samples, bpnBootSample{
		At: fixture.nodes[0].rebootObservedAt.Add(time.Second), Node: "worker-0", ID: "another-reboot",
	})
	if err := fixture.checkRestorationBoots(); err == nil {
		t.Fatal("a second target reboot must fail restoration")
	}
}

func TestBPNTaintCheckPreservesUnrelatedTaints(t *testing.T) {
	baseline := []corev1.Taint{
		{Key: "administrator.example/retain", Value: "original", Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/out-of-service", Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute},
	}
	if !bpnSameTaints([]corev1.Taint{baseline[1], baseline[0]}, baseline) {
		t.Fatal("taint order must not change the comparison")
	}
	changed := append([]corev1.Taint(nil), baseline...)
	changed[0].Value = "changed"
	if bpnSameTaints(changed, baseline) {
		t.Fatal("an unrelated taint change must fail restoration")
	}
	if bpnSameTaints(baseline[1:], baseline) {
		t.Fatal("a removed original taint must fail restoration")
	}
}

func TestBPNAdministratorCleanupPreservesExistingFixtureTaint(t *testing.T) {
	fixture := bpnAdministratorTaints()[0]
	initial := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{fixture}}}
	if bpnOwnedAdministratorTaint(fixture, initial) {
		t.Fatal("cleanup must not remove a taint that existed before the test")
	}
	if !bpnOwnedAdministratorTaint(fixture, &corev1.Node{}) {
		t.Fatal("cleanup must identify the fixture added by this test")
	}
}

type bpnPhaseNodes struct {
	typedcorev1.NodeInterface
	nodes     map[string]*corev1.Node
	deadlines []time.Time
	checked   []string
}

func (nodes *bpnPhaseNodes) Get(ctx context.Context, name string, _ metav1.GetOptions) (*corev1.Node, error) {
	deadline, _ := ctx.Deadline()
	nodes.deadlines = append(nodes.deadlines, deadline)
	nodes.checked = append(nodes.checked, name)

	return nodes.nodes[name].DeepCopy(), nil
}

func (nodes *bpnPhaseNodes) Update(ctx context.Context, node *corev1.Node,
	_ metav1.UpdateOptions,
) (*corev1.Node, error) {
	deadline, _ := ctx.Deadline()
	nodes.deadlines = append(nodes.deadlines, deadline)
	nodes.nodes[node.Name] = node.DeepCopy()

	return node.DeepCopy(), nil
}

func bpnUseFakeAPIWithTestLabeledNodes(t *testing.T) (*bpnFixture, *bpnPhaseNodes) {
	t.Helper()
	fixture := &bpnFixture{name: "owned"}
	nodes := &bpnPhaseNodes{nodes: map[string]*corev1.Node{}}
	for index := range 3 {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("worker-%d", index), UID: types.UID(fmt.Sprintf("uid-%d", index)),
		}}
		fixture.nodes = append(fixture.nodes, &bpnNode{initial: node.DeepCopy()})
		node.Labels = map[string]string{"tests.medik8s.io/block-per-node": fixture.name}
		nodes.nodes[node.Name] = node
	}
	originalClient := APIClient
	APIClient = &clients.Settings{CoreV1Interface: bpnCleanupCore{nodes: nodes}}
	t.Cleanup(func() { APIClient = originalClient })

	return fixture, nodes
}

func TestBPNNodeCleanupChecksAllNodesWithinOneDeadline(t *testing.T) {
	fixture, nodes := bpnUseFakeAPIWithTestLabeledNodes(t)
	nodes.nodes["worker-0"].UID = "replacement"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := bpnCleanupPollWithin(ctx, fixture.checkAllNodesReadyAndBootIDsMatch)
	if !bpnIsPermanent(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("changed identity must stop this phase promptly: %v", err)
	}
	if len(nodes.checked) != 3 {
		t.Fatalf("all three Nodes must be checked: %v", nodes.checked)
	}
	for _, observed := range nodes.deadlines {
		if !observed.Equal(deadline) {
			t.Fatalf("Node checks must share deadline %s, observed %s", deadline, observed)
		}
	}
	for _, name := range []string{"worker-0", "worker-1", "worker-2"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("cleanup must retain each Node failure: %v", err)
		}
	}
}

func TestBPNLabelsRestoreOtherNodesAfterPermanentFailure(t *testing.T) {
	fixture, nodes := bpnUseFakeAPIWithTestLabeledNodes(t)
	const key = "tests.medik8s.io/block-per-node"
	nodes.nodes["worker-0"].UID = "replacement"
	fixture.nodes[2].initial.Labels = map[string]string{key: "original"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := fixture.restoreWorkerLabels(ctx)
	if !bpnIsPermanent(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("permanent label failure must remain a failure: %v", err)
	}
	if nodes.nodes["worker-0"].Labels[key] != fixture.name {
		t.Fatal("cleanup must not modify a replacement Node")
	}
	if _, present := nodes.nodes["worker-1"].Labels[key]; present {
		t.Fatal("cleanup must still remove the second Node's owned label")
	}
	if nodes.nodes["worker-2"].Labels[key] != "original" {
		t.Fatal("cleanup must still restore the third Node's original label")
	}
	for _, observed := range nodes.deadlines {
		if !observed.Equal(deadline) {
			t.Fatalf("label checks and updates must share deadline %s, observed %s", deadline, observed)
		}
	}
}

func TestBPNCleanupPollRetriesTransientErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	transient := errors.New("API temporarily unavailable")
	err := bpnCleanupPollWithin(ctx, func(context.Context) error { return transient })
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, transient) || bpnIsPermanent(err) {
		t.Fatalf("a transient error must retain the retry deadline: %v", err)
	}
}

type bpnPhaseFixtures struct {
	client.Client
	objects   map[string]*unstructured.Unstructured
	deleted   []string
	deadlines []time.Time
}

func (fixtures *bpnPhaseFixtures) Get(ctx context.Context, key client.ObjectKey, object client.Object,
	_ ...client.GetOption,
) error {
	deadline, _ := ctx.Deadline()
	fixtures.deadlines = append(fixtures.deadlines, deadline)
	current := fixtures.objects[key.Name]
	if current == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "fixtures"}, key.Name)
	}
	target, ok := object.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected fixture type %T", object)
	}
	current.DeepCopyInto(target)

	return nil
}

func (fixtures *bpnPhaseFixtures) Delete(ctx context.Context, object client.Object,
	options ...client.DeleteOption,
) error {
	deadline, _ := ctx.Deadline()
	fixtures.deadlines = append(fixtures.deadlines, deadline)
	deleteOptions := &client.DeleteOptions{}
	for _, option := range options {
		option.ApplyToDelete(deleteOptions)
	}
	if deleteOptions.Preconditions == nil || deleteOptions.Preconditions.UID == nil ||
		*deleteOptions.Preconditions.UID != object.GetUID() {
		return errors.New("fixture deletion needs the original UID precondition")
	}
	fixtures.deleted = append(fixtures.deleted, object.GetName())
	if len(object.GetFinalizers()) == 0 {
		delete(fixtures.objects, object.GetName())
	}

	return nil
}

func TestBPNFixtureDeletionSharesOnePhase(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%t", held), func(t *testing.T) {
			fixture := &bpnFixture{}
			api := &bpnPhaseFixtures{objects: map[string]*unstructured.Unstructured{}}
			for index, name := range []string{"nhc", "template", "config"} {
				object := bpnNewPendingSBR()
				object.SetName(name)
				object.SetUID(types.UID(name))
				if held && index == 0 {
					object.SetFinalizers([]string{"preserve"})
				}
				api.objects[name] = object.DeepCopy()
				switch index {
				case 0:
					fixture.nhc = object
				case 1:
					fixture.template = object
				case 2:
					fixture.config = object
				}
			}
			originalClient := APIClient
			APIClient = &clients.Settings{Client: api}
			t.Cleanup(func() { APIClient = originalClient })
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			deadline, _ := ctx.Deadline()
			err := fixture.deleteFixtures(ctx, nil)
			if (err != nil) != held {
				t.Fatalf("normal deletion result = %v, held finalizer = %t", err, held)
			}
			if len(api.deleted) != 3 {
				t.Fatalf("all deletions must start without a serial wait: %v", api.deleted)
			}
			if held && len(api.objects["nhc"].GetFinalizers()) != 1 {
				t.Fatal("normal deletion must preserve finalizers")
			}
			for _, observed := range api.deadlines {
				if !observed.Equal(deadline) {
					t.Fatalf("fixture calls must share deadline %s, observed %s", deadline, observed)
				}
			}
		})
	}
}

func TestBPNFixturePhaseWaitsForOwnedOperands(t *testing.T) {
	claim := bpnNewObjectWithOwnerUID("PersistentVolumeClaim", "probe", "claim", "config")
	fixture, _ := bpnUseFakeAPIWithOwnedOperands(t, claim)
	fixture.config = bpnNewPendingSBR()
	fixture.config.SetName("config")
	fixture.config.SetUID("config")
	api := &bpnPhaseFixtures{objects: map[string]*unstructured.Unstructured{"config": fixture.config.DeepCopy()}}
	APIClient.Client = api
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := fixture.deleteFixtures(ctx, map[types.UID]bool{"config": true, "claim": true})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("the shared fixture phase must wait for its owned claim: %v", err)
	}
	if len(api.objects) != 0 || len(api.deleted) != 1 {
		t.Fatal("fixture deletion must finish before waiting for its remaining operands")
	}
}

func TestBPNRecoveryRequiresReadyOperandsOnEachWorker(t *testing.T) {
	agent := bpnNewReadyOperandPod(sbrparams.AgentContainerName)
	reporter := bpnNewReadyOperandPod("sbr-boot-reporter")
	running := map[string]bool{"current-" + sbrparams.AgentContainerName: true, "current-sbr-boot-reporter": true}
	for _, test := range []struct {
		name   string
		pod    corev1.Pod
		change func(*corev1.Pod)
	}{
		{name: "missing agent", pod: agent, change: func(pod *corev1.Pod) { pod.Spec.Containers = nil }},
		{name: "missing reporter", pod: reporter, change: func(pod *corev1.Pod) { pod.Spec.Containers = nil }},
		{name: "unready agent", pod: agent, change: func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].Ready = false }},
		{name: "unready reporter", pod: reporter, change: func(pod *corev1.Pod) {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
		}},
		{name: "terminating agent", pod: agent, change: func(pod *corev1.Pod) {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}},
		{name: "terminating reporter", pod: reporter, change: func(pod *corev1.Pod) {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}},
		{name: "stale agent container", pod: agent, change: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].ContainerID = "cri-o://prior-agent"
		}},
		{name: "stale reporter container", pod: reporter, change: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].ContainerID = "cri-o://prior-reporter"
		}},
		{name: "different configuration", pod: agent, change: func(pod *corev1.Pod) { pod.Labels["sbrconfig"] = "other" }},
		{name: "different worker", pod: reporter, change: func(pod *corev1.Pod) { pod.Spec.NodeName = "worker-0" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			pods := []corev1.Pod{*agent.DeepCopy(), *reporter.DeepCopy()}
			for index := range pods {
				if pods[index].Name == test.pod.Name {
					test.change(&pods[index])
				}
			}
			if err := bpnCheckNodeOperands(pods, "owned", "worker-4", running, false); err == nil {
				t.Fatal("invalid operand status passed recovery")
			}
		})
	}
	if err := bpnCheckNodeOperands([]corev1.Pod{agent, reporter}, "owned", "worker-4", running, false); err != nil {
		t.Fatal(err)
	}
}

func bpnNewReadyOperandPod(containerName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: containerName, Namespace: medik8sparams.OperatorNs, Labels: map[string]string{"sbrconfig": "owned"},
		},
		Spec: corev1.PodSpec{NodeName: "worker-4", Containers: []corev1.Container{{Name: containerName}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: containerName, ContainerID: "cri-o://current-" + containerName, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func TestBPNRecoveryChecksFifthWorkersNodeState(t *testing.T) {
	fixture, nodes := bpnUseFakeAPIWithTestLabeledNodes(t)
	for index := range 5 {
		name := fmt.Sprintf("worker-%d", index)
		node := bpnNewReadyLinuxWorker(name)
		node.UID = types.UID(name)
		node.Status.NodeInfo.BootID = name + "-boot"
		node.Status.Conditions = append(node.Status.Conditions,
			corev1.NodeCondition{Type: sbrparams.SBRStorageUnhealthyCondition, Status: corev1.ConditionFalse})
		nodes.nodes[name] = &node
		expected := &bpnNode{initial: node.DeepCopy(), boot: node.Status.NodeInfo.BootID}
		if index < len(fixture.nodes) {
			fixture.nodes[index] = expected
		} else {
			fixture.nodes = append(fixture.nodes, expected)
		}
	}
	fixture.adminAdded = true
	target := nodes.nodes["worker-0"]
	target.Spec.Unschedulable = true
	target.Spec.Taints = bpnAdministratorTaints()
	check := func() error { return fixture.checkWorkerRecoveryInAPI(t.Context(), corev1.ConditionFalse) }
	if err := check(); err != nil {
		t.Fatalf("recovery must preserve administrator restrictions: %v", err)
	}
	fifth := nodes.nodes["worker-4"]
	for _, test := range []struct {
		name   string
		change func(*corev1.Node)
	}{
		{name: "not ready", change: func(node *corev1.Node) { node.Status.Conditions[0].Status = corev1.ConditionFalse }},
		{name: "unhealthy storage", change: func(node *corev1.Node) {
			node.Status.Conditions[1].Status = corev1.ConditionTrue
		}},
		{name: "still cordoned", change: func(node *corev1.Node) { node.Spec.Unschedulable = true }},
		{name: "retained OutOfService", change: func(node *corev1.Node) { node.Spec.Taints = bpnAdministratorTaints()[:1] }},
		{name: "different boot ID", change: func(node *corev1.Node) { node.Status.NodeInfo.BootID = "unexpected" }},
		{name: "different UID", change: func(node *corev1.Node) { node.UID = "replacement" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodes.nodes[fifth.Name] = fifth.DeepCopy()
			test.change(nodes.nodes[fifth.Name])
			if err := check(); err == nil || !strings.Contains(err.Error(), fifth.Name) {
				t.Fatalf("recovery must report fifth worker failure, got %v", err)
			}
		})
	}
	nodes.nodes[fifth.Name] = fifth
	target.Status.Conditions[1].Status = corev1.ConditionTrue
	if err := fixture.checkWorkerRecoveryInAPI(t.Context(), corev1.ConditionTrue); err != nil {
		t.Fatalf("partial recovery must allow the intentional target storage failure: %v", err)
	}
	target.Spec.Taints = target.Spec.Taints[:1]
	if err := fixture.checkWorkerRecoveryInAPI(t.Context(), corev1.ConditionTrue); err == nil {
		t.Fatal("recovery must reject removal of administrator restrictions")
	}
}

func TestBPNRecoveryRejectsAPISuccessAfterDeadline(t *testing.T) {
	node := bpnNewReadyLinuxWorker("worker-a")
	node.Status.NodeInfo.BootID = "expected"
	node.Status.Conditions = append(node.Status.Conditions,
		corev1.NodeCondition{Type: sbrparams.SBRStorageUnhealthyCondition, Status: corev1.ConditionFalse})
	fixture := &bpnFixture{nodes: []*bpnNode{{initial: node.DeepCopy(), boot: "expected"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	nodes := bpnCleanupNodes{get: func(call context.Context) (*corev1.Node, error) {
		if observed, _ := call.Deadline(); !observed.Equal(deadline) {
			t.Errorf("node call deadline = %s, want %s", observed, deadline)
		}
		<-call.Done()

		return node.DeepCopy(), nil
	}}
	originalClient := APIClient
	APIClient = &clients.Settings{CoreV1Interface: bpnCleanupCore{nodes: nodes}}
	t.Cleanup(func() { APIClient = originalClient })
	if err := fixture.checkWorkerRecoveryWithinPoll(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recovery accepted late API success: %v", err)
	}
}

func TestBPNPartialRecoveryAcceptsRunningTargetAgentWithUnhealthyStorage(t *testing.T) {
	agent := bpnNewReadyOperandPod(sbrparams.AgentContainerName)
	reporter := bpnNewReadyOperandPod("sbr-boot-reporter")
	agent.Status.Conditions[0].Status = corev1.ConditionFalse
	agent.Status.ContainerStatuses[0].Ready = false
	running := map[string]bool{
		agent.Status.ContainerStatuses[0].ContainerID[len("cri-o://"):]:    true,
		reporter.Status.ContainerStatuses[0].ContainerID[len("cri-o://"):]: true,
	}
	pods := []corev1.Pod{agent, reporter}
	if err := bpnCheckNodeOperands(pods, "owned", "worker-4", running, true); err != nil {
		t.Fatalf("running target agent must pass partial recovery: %v", err)
	}
	if err := bpnCheckNodeOperands(pods, "owned", "worker-4", running, false); err == nil {
		t.Fatal("unready target agent must fail full recovery")
	}
	pods[1].Status.ContainerStatuses[0].Ready = false
	if err := bpnCheckNodeOperands(pods, "owned", "worker-4", running, true); err == nil {
		t.Fatal("unready target reporter must fail partial recovery")
	}
	pods[1].Status.ContainerStatuses[0].Ready = true
	for _, id := range []string{"cri-o://prior-agent", ""} {
		pods[0].Status.ContainerStatuses[0].ContainerID = id
		if err := bpnCheckNodeOperands(pods, "owned", "worker-4", running, true); err == nil {
			t.Fatalf("partial recovery accepted a target agent with ID %q", id)
		}
	}
}

func TestBPNRecoveryRequiresNoCurrentRequestsOnSelectedWorkers(t *testing.T) {
	request := bpnNewPendingSBR()
	request.SetName("worker-4")
	request.SetNamespace("another-namespace")
	unselected := bpnNewPendingSBR()
	unselected.SetName("unselected-worker")
	resource := request.GroupVersionKind().GroupVersion().WithResource("storagebasedremediations")
	api := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{resource: "StorageBasedRemediationList"}, request, unselected)
	originalClient := APIClient
	APIClient = &clients.Settings{Interface: api}
	t.Cleanup(func() { APIClient = originalClient })
	fixture := &bpnFixture{}
	for index := range 5 {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%d", index)}}
		fixture.nodes = append(fixture.nodes, &bpnNode{initial: node})
	}
	err := fixture.checkNoCurrentWorkerRequests(t.Context())
	if err == nil || !strings.Contains(err.Error(), request.GetName()) {
		t.Fatalf("recovery must report the remaining request on the fifth worker: %v", err)
	}
	if err := api.Resource(resource).Namespace(request.GetNamespace()).Delete(t.Context(),
		request.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkNoCurrentWorkerRequests(t.Context()); err != nil {
		t.Fatalf("request for an unselected worker must not block recovery: %v", err)
	}
}
