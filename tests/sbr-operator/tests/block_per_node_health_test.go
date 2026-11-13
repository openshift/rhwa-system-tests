package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
)

func bpnHealthTestCase(t *testing.T, currentStatus corev1.ConditionStatus) (*bpnFixture, time.Time, *fake.Clientset) {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "worker-original"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeConditionType(sbrparams.SBRStorageUnhealthyCondition), Status: corev1.ConditionTrue,
		}}},
	}
	watcher := watch.NewRaceFreeFake()
	recorder, err := helpers.StartObjectRecorder(helpers.RecorderConfig[*corev1.Node]{
		List:  func(context.Context) ([]*corev1.Node, string, error) { return []*corev1.Node{node}, "1", nil },
		Watch: func(context.Context, string) (watch.Interface, error) { return watcher, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recorder.Stop)
	previousAPI := medik8sinittools.APIClient
	current := node.DeepCopy()
	current.Status.Conditions[0].Status = currentStatus
	api := fake.NewSimpleClientset(current)
	medik8sinittools.APIClient = &clients.Settings{CoreV1Interface: api.CoreV1()}
	t.Cleanup(func() { medik8sinittools.APIClient = previousAPI })
	caseRun := &bpnFixture{
		ctx: context.Background(), nodes: []*bpnNode{{initial: node, recorder: recorder}},
		healthFaults: map[int]sbrutils.BlockFaultEvidence{},
	}

	return caseRun, recorder.Snapshot().Observations[0].At, api
}

func TestBPNUnhealthyRejectsLateObservedState(t *testing.T) {
	caseRun, observedAt, api := bpnHealthTestCase(t, corev1.ConditionTrue)
	active := observedAt.Add(-sbrparams.BPNHealthChange - time.Minute)
	caseRun.healthFaults[0] = sbrutils.BlockFaultEvidence{DispatchedAt: active.Add(-time.Second), ActiveAt: active}
	if _, err := caseRun.waitUnhealthy(0, nil); err == nil {
		t.Fatal("a late current True state must not prove health changed within the fault deadline")
	}
	if len(api.Actions()) != 0 {
		t.Fatal("a missed receipt deadline must fail before a live GET can provide late evidence")
	}
}

func TestBPNUnhealthyRequiresCurrentTrueState(t *testing.T) {
	caseRun, observedAt, _ := bpnHealthTestCase(t, corev1.ConditionFalse)
	caseRun.healthFaults[0] = sbrutils.BlockFaultEvidence{
		DispatchedAt: observedAt.Add(-time.Second), ActiveAt: observedAt,
	}
	firstObserved, err := caseRun.waitUnhealthy(0, nil)
	if err == nil || !firstObserved.Equal(observedAt) {
		t.Fatalf("timely history must not conceal a current False state: first=%s, err=%v", firstObserved, err)
	}
}

func TestBPNUnhealthyPreservesEvidenceAfterLaterFaultPreparation(t *testing.T) {
	caseRun, observedAt, _ := bpnHealthTestCase(t, corev1.ConditionTrue)
	deadline := observedAt.Add(time.Millisecond)
	active := deadline.Add(-sbrparams.BPNHealthChange)
	caseRun.healthFaults[0] = sbrutils.BlockFaultEvidence{DispatchedAt: active.Add(-time.Second), ActiveAt: active}
	timer := time.NewTimer(time.Until(deadline) + time.Millisecond)
	defer timer.Stop()
	<-timer.C
	firstObserved, err := caseRun.waitUnhealthy(0, nil)
	if err != nil || !firstObserved.Equal(observedAt) {
		t.Fatalf("timely history must survive a later assertion: first=%s, err=%v", firstObserved, err)
	}
}

func bpnHealthSample(observedAt time.Time) helpers.RecorderObservation[*corev1.Node] {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "worker-original"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeConditionType(sbrparams.SBRStorageUnhealthyCondition), Status: corev1.ConditionTrue,
		}}},
	}

	return helpers.RecorderObservation[*corev1.Node]{
		At: observedAt, UID: node.UID, Object: node, EventType: watch.Modified,
	}
}

func TestBPNUnhealthyUsesEachNodesFaultDeadline(t *testing.T) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	observedAt := start.Add(sbrparams.BPNHealthChange + time.Second)
	samples := []helpers.RecorderObservation[*corev1.Node]{bpnHealthSample(observedAt)}
	first := sbrutils.BlockFaultEvidence{DispatchedAt: start.Add(-time.Second), ActiveAt: start}
	second := sbrutils.BlockFaultEvidence{
		DispatchedAt: start.Add(time.Minute - time.Second), ActiveAt: start.Add(time.Minute),
	}
	if _, err := bpnUnhealthyEvidenceFrom(samples, "worker-original", first); err == nil {
		t.Fatal("later Node preparation must not extend the first Node's health deadline")
	}
	if received, err := bpnUnhealthyEvidenceFrom(samples, "worker-original", second); err != nil ||
		!received.Equal(observedAt) {
		t.Fatalf("the second Node retains its own deadline: received=%s, err=%v", received, err)
	}
}

func TestBPNUnhealthyUsesFirstObservationFromCurrentFaultCycle(t *testing.T) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	fault := sbrutils.BlockFaultEvidence{DispatchedAt: start, ActiveAt: start.Add(time.Second)}
	first := bpnHealthSample(start.Add(time.Millisecond))
	later := bpnHealthSample(start.Add(2 * sbrparams.BPNHealthChange))
	if received, err := bpnUnhealthyEvidenceFrom(
		[]helpers.RecorderObservation[*corev1.Node]{later, first}, "worker-original", fault); err != nil ||
		!received.Equal(first.At) {
		t.Fatalf("confirmation must preserve the first current-cycle observation: received=%s, err=%v", received, err)
	}
	stale := bpnHealthSample(start.Add(-time.Millisecond))
	if _, err := bpnUnhealthyEvidenceFrom(
		[]helpers.RecorderObservation[*corev1.Node]{stale}, "worker-original", fault); err == nil {
		t.Fatal("a previous fault cycle must not satisfy a repeated activation")
	}
}

func TestBPNUnhealthyRejectsUnrelatedAndDeletedEvidence(t *testing.T) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	fault := sbrutils.BlockFaultEvidence{DispatchedAt: start, ActiveAt: start.Add(time.Second)}
	for _, invalid := range []string{"replacement UID", "deleted Node", "missing object", "healthy condition"} {
		t.Run(invalid, func(t *testing.T) {
			sample := bpnHealthSample(start.Add(time.Second))
			switch invalid {
			case "replacement UID":
				sample.UID = "replacement"
			case "deleted Node":
				sample.EventType = watch.Deleted
			case "missing object":
				sample.Object = nil
			case "healthy condition":
				sample.Object.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			if _, err := bpnUnhealthyEvidenceFrom(
				[]helpers.RecorderObservation[*corev1.Node]{sample}, "worker-original", fault); err == nil {
				t.Fatal("unrelated or absent state must not prove a timely unhealthy condition")
			}
		})
	}
}

func TestBPNUnhealthyChecksNegativeEvidenceBeforeHealth(t *testing.T) {
	caseRun, observedAt, api := bpnHealthTestCase(t, corev1.ConditionTrue)
	caseRun.healthFaults[0] = sbrutils.BlockFaultEvidence{
		DispatchedAt: observedAt.Add(-time.Second), ActiveAt: observedAt,
	}
	prohibited := errors.New("a reboot occurred during health detection")
	_, err := caseRun.waitUnhealthy(0, func() error { return prohibited })
	if !errors.Is(err, prohibited) {
		t.Fatalf("health detection must preserve the negative evidence failure: %v", err)
	}
	if len(api.Actions()) != 0 {
		t.Fatal("prohibited evidence must stop the health wait before another API read")
	}
}

func TestBPNStorageObservationUsesLastConfirmedActivation(t *testing.T) {
	start := time.Now().Add(-2 * time.Minute)
	faults := map[int]sbrutils.BlockFaultEvidence{
		0: {DispatchedAt: start.Add(-time.Second), ActiveAt: start},
		1: {DispatchedAt: start, ActiveAt: start.Add(10 * time.Second)},
		2: {DispatchedAt: start, ActiveAt: start.Add(20 * time.Second)},
	}
	deadline, err := bpnStorageObservationDeadline(faults, 0, 1, 2)
	if err != nil || !deadline.Equal(faults[2].ActiveAt.Add(sbrparams.BPNNoReboot)) {
		t.Fatalf("broad fault deadline must use the last activation: %s, %v", deadline, err)
	}
	deadline, err = bpnStorageObservationDeadline(faults, 0)
	if err != nil || !deadline.Equal(start.Add(sbrparams.BPNNoReboot)) {
		t.Fatalf("single fault deadline must include elapsed health detection: %s, %v", deadline, err)
	}
	for _, indices := range [][]int{nil, {3}, {0, 3}} {
		if _, err := bpnStorageObservationDeadline(faults, indices...); err == nil {
			t.Fatalf("missing activation evidence must fail: %v", indices)
		}
	}
}
