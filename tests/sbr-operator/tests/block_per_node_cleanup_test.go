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
