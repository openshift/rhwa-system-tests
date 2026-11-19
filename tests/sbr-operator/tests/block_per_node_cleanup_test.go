package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

func bpnCleanupRequest() *unstructured.Unstructured {
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
	if err := fixture.releaseRequestWithin(ctx, bpnCleanupRequest()); !errors.Is(err, context.DeadlineExceeded) {
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
		requests:      []*unstructured.Unstructured{bpnCleanupRequest()},
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
		for index := range 3 {
			fixture.nodes = append(fixture.nodes, &bpnNode{
				initial: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%d", index)}}, boot: "expected",
			})
		}
		fixture.nodes[0].rebootObservedAt = baseline.Add(30 * time.Second)
		fixture.samples = []bpnBootSample{{At: baseline.Add(sampleOffset), Node: "worker-1", ID: "unexpected"}}
		if err := fixture.finalRestoredObservation(); err == nil || !strings.Contains(err.Error(), "worker-1 rebooted") {
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

func bpnPhaseFixture(t *testing.T) (*bpnFixture, *bpnPhaseNodes) {
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
	fixture, nodes := bpnPhaseFixture(t)
	nodes.nodes["worker-0"].UID = "replacement"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := bpnCleanupPollWithin(ctx, fixture.restoreNodes)
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
	fixture, nodes := bpnPhaseFixture(t)
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
				object := bpnCleanupRequest()
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
	claim := bpnOwnershipObject("PersistentVolumeClaim", "probe", "claim", "config")
	fixture, _ := bpnOwnershipFixture(t, claim)
	fixture.config = bpnCleanupRequest()
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
