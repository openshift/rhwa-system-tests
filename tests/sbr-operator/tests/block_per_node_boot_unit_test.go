package tests

import (
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBPNBootOrderingRequiresBoundedKubeletStart(t *testing.T) {
	const bootStart = 5 * time.Second
	const timeout = sbrparams.BPNBootUnitTimeout
	for _, testCase := range []struct {
		name     string
		exit     time.Duration
		kubelet  time.Duration
		timedOut bool
		wantFail bool
	}{
		{"successful cordon", bootStart + time.Second, bootStart + 2*time.Second, false, false},
		{"timeout", bootStart + timeout, bootStart + timeout + time.Second, true, false},
		{"early expiry", bootStart + time.Second, bootStart + 2*time.Second, true, true},
		{"kubelet starts first", bootStart + time.Second, bootStart - time.Second, false, true},
		{"unit exceeds timeout", bootStart + timeout + time.Minute, bootStart + timeout + 2*time.Minute, false, true},
		{"kubelet starts late", bootStart + timeout, bootStart + timeout + time.Minute, true, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			timing := bpnBootUnitTiming{Start: bootStart, Exit: testCase.exit, KubeletStart: testCase.kubelet}
			if err := bpnCheckBootOrdering(timing, testCase.timedOut); (err != nil) != testCase.wantFail {
				t.Fatalf("bpnCheckBootOrdering(%+v, %t) = %v", timing, testCase.timedOut, err)
			}
		})
	}
}

func TestBPNRejoinHistoryRequiresOwnershipUntilRelease(t *testing.T) {
	start := time.Date(2026, time.November, 23, 8, 0, 0, 0, time.UTC)
	earliest := start.Add(time.Minute)
	end := earliest.Add(2 * time.Second)
	held := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "target", UID: "target-uid", Annotations: map[string]string{bpnRejoinCordon: ""}},
		Spec:       corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: bpnRejoinPending, Status: corev1.ConditionTrue},
		}},
	}
	conditionCleared := held.DeepCopy()
	conditionCleared.Status.Conditions[0].Status = corev1.ConditionFalse
	released := conditionCleared.DeepCopy()
	delete(released.Annotations, bpnRejoinCordon)
	released.Spec.Unschedulable = false
	unkeyed := held.DeepCopy()
	delete(unkeyed.Annotations, bpnRejoinCordon)
	sample := func(node *corev1.Node, at time.Time) helpers.RecorderObservation[*corev1.Node] {
		return helpers.RecorderObservation[*corev1.Node]{Object: node, At: at, UID: node.UID}
	}
	for _, testCase := range []struct {
		name     string
		updates  []helpers.RecorderObservation[*corev1.Node]
		wantFail bool
	}{
		{
			"ordered release", []helpers.RecorderObservation[*corev1.Node]{
				sample(held, start), sample(conditionCleared, earliest), sample(released, end),
			}, false,
		},
		{
			"early clear", []helpers.RecorderObservation[*corev1.Node]{
				sample(held, start), sample(conditionCleared, start.Add(time.Second)), sample(released, end),
			}, true,
		},
		{
			"condition and cordon removed together", []helpers.RecorderObservation[*corev1.Node]{
				sample(held, start), sample(released, end),
			}, true,
		},
		{
			"ownership cleared before cordon", []helpers.RecorderObservation[*corev1.Node]{
				sample(held, start), sample(unkeyed, earliest), sample(released, end),
			}, true,
		},
		{"no initial evidence", nil, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := bpnCheckRejoinHistory(testCase.updates, held.UID, start, end, earliest)
			if (err != nil) != testCase.wantFail {
				t.Fatalf("bpnCheckRejoinHistory() = %v", err)
			}
		})
	}
}

func TestBPNBaselineRestrictionsRejectResidualRejoinOwnership(t *testing.T) {
	baseline := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "target"}, Spec: corev1.NodeSpec{Unschedulable: true}}
	fixture := &bpnFixture{nodes: []*bpnNode{{initial: baseline}}}
	if err := fixture.checkNodeRestrictions(0, baseline); err != nil {
		t.Fatalf("the administrator cordon must pass: %v", err)
	}
	current := baseline.DeepCopy()
	current.Annotations = map[string]string{bpnRejoinCordon: ""}
	if err := fixture.checkNodeRestrictions(0, current); err == nil {
		t.Fatal("an empty ownership key must fail recovery")
	}
	delete(current.Annotations, bpnRejoinCordon)
	current.Status.Conditions = []corev1.NodeCondition{{Type: bpnRejoinPending, Status: corev1.ConditionUnknown}}
	if err := fixture.checkNodeRestrictions(0, current); err == nil {
		t.Fatal("an unknown pending condition must fail recovery")
	}
	current.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := fixture.checkNodeRestrictions(0, current); err != nil {
		t.Fatalf("a cleared pending condition must pass: %v", err)
	}
}
