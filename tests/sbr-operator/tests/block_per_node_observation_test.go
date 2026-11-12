package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

func bpnResultFixture() (*unstructured.Unstructured, *corev1.Node, time.Time) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "node-original"}}
	request := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": node.Name, "namespace": "operator", "uid": "request-original"},
		"status": map[string]interface{}{
			"cleanup": map[string]interface{}{
				"startedAt": start.Format(time.RFC3339Nano),
				"deadline":  start.Add(sbrparams.BPNCleanupPeriod).Format(time.RFC3339Nano),
				"phase":     "Incomplete",
			},
			"conditions": []interface{}{
				map[string]interface{}{"type": "FencingSucceeded", "status": "True"},
				map[string]interface{}{"type": "Succeeded", "status": "True"},
				map[string]interface{}{"type": "Processing", "status": "False"},
			},
		},
	}}

	return request, node, start
}

func bpnResultSample(
	object *unstructured.Unstructured, at time.Time,
) helpers.RecorderObservation[*unstructured.Unstructured] {
	return helpers.RecorderObservation[*unstructured.Unstructured]{
		Object: object, At: at, UID: object.GetUID(), EventType: watch.Modified,
	}
}

func TestBPNTerminalRequestedResultRequiresCleanupPhase(t *testing.T) {
	for _, phase := range []string{"Completed", "Incomplete", "Unknown", "", "Pending", "Skipped"} {
		t.Run(phase, func(t *testing.T) {
			request, _, _ := bpnResultFixture()
			if err := unstructured.SetNestedField(request.Object, phase, "status", "cleanup", "phase"); err != nil {
				t.Fatal(err)
			}
			err := bpnTerminalRequestedResult(request)
			valid := phase == "Completed" || phase == "Incomplete" || phase == "Unknown"
			if (err == nil) != valid {
				t.Fatalf("phase %q: got %v, valid=%v", phase, err, valid)
			}
		})
	}
}

func TestBPNRequestIdentityUsesOriginalNode(t *testing.T) {
	for _, field := range []string{"unchanged", "uid", "name", "namespace", "nodeUID"} {
		t.Run(field, func(t *testing.T) {
			request, node, _ := bpnResultFixture()
			current := request.DeepCopy()
			switch field {
			case "uid":
				current.SetUID("replacement")
			case "name":
				current.SetName("other-worker")
			case "namespace":
				current.SetNamespace("other-operator")
			case "nodeUID":
				if err := unstructured.SetNestedField(current.Object, "replacement-node", "status", "nodeUID"); err != nil {
					t.Fatal(err)
				}
			}
			err := bpnRequestIdentity(current, request, node)
			if (err == nil) != (field == "unchanged") {
				t.Fatalf("field %s: got %v", field, err)
			}
		})
	}
}

func TestBPNCleanupHistoryRejectsTemporaryTimestampChanges(t *testing.T) {
	for _, field := range []string{"startedAt", "deadline"} {
		for _, action := range []string{"change", "remove"} {
			t.Run(field+"/"+action, func(t *testing.T) {
				request, node, start := bpnResultFixture()
				changed := request.DeepCopy()
				if action == "remove" {
					unstructured.RemoveNestedField(changed.Object, "status", "cleanup", field)
				} else if err := unstructured.SetNestedField(changed.Object,
					start.Add(time.Hour).Format(time.RFC3339Nano), "status", "cleanup", field); err != nil {
					t.Fatal(err)
				}
				samples := []helpers.RecorderObservation[*unstructured.Unstructured]{
					bpnResultSample(request, start), bpnResultSample(changed, start.Add(time.Second)),
					bpnResultSample(request.DeepCopy(), start.Add(2*time.Second)),
				}
				evidence := bpnCleanupEvidence{startedAt: start, deadline: start.Add(sbrparams.BPNCleanupPeriod)}
				if err := bpnCleanupHistory(samples, request, node, evidence); err == nil {
					t.Fatal("temporary timestamp change must fail even after restoration")
				}
			})
		}
	}
}

func TestBPNCleanupEvidenceUsesObservationTime(t *testing.T) {
	request, node, start := bpnResultFixture()
	publishedBy := start.Add(sbrparams.BPNRelease)
	terminalAt := start.Add(sbrparams.BPNCleanupPeriod)
	early := request.DeepCopy()
	unstructured.RemoveNestedField(early.Object, "status", "conditions")
	samples := []helpers.RecorderObservation[*unstructured.Unstructured]{
		bpnResultSample(early, start), bpnResultSample(request, terminalAt),
	}
	evidence, err := bpnCleanupEvidenceFrom(samples, request, node, publishedBy)
	if err != nil || !evidence.publishedAt.Equal(start) || !evidence.terminalAt.Equal(terminalAt) {
		t.Fatalf("stored early evidence must remain valid after a later startup wait: %+v, %v", evidence, err)
	}
	late := []helpers.RecorderObservation[*unstructured.Unstructured]{
		bpnResultSample(request, publishedBy.Add(time.Second)),
	}
	if _, err := bpnCleanupEvidenceFrom(late, request, node, publishedBy); err == nil {
		t.Fatal("a late GET must not prove early cleanup publication")
	}
}

func TestBPNRetainedCleanupRejectsEarlySuccess(t *testing.T) {
	request, _, start := bpnResultFixture()
	deadline := start.Add(sbrparams.BPNCleanupPeriod)
	if err := bpnRetainedCleanupResult(request, deadline.Add(-time.Second), deadline); err == nil {
		t.Fatal("a retained Pod must prevent success before the stored deadline")
	}
	if err := bpnRetainedCleanupResult(request, deadline, deadline); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(request.Object, "Completed", "status", "cleanup", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := bpnRetainedCleanupResult(request, deadline, deadline); err == nil {
		t.Fatal("a retained Pod must prevent cleanup Completed")
	}
}

func TestBPNObservationRejectsUnhealthyBaselineWithoutLaterEvents(t *testing.T) {
	for _, unhealthy := range []bool{false, true} {
		t.Run(fmt.Sprintf("unhealthy=%t", unhealthy), func(t *testing.T) {
			caseRun := &bpnFixture{}
			for nodeIndex := range 3 {
				status := corev1.ConditionFalse
				if unhealthy && nodeIndex == 0 {
					status = corev1.ConditionTrue
				}
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("worker-%d", nodeIndex), UID: types.UID(fmt.Sprintf("uid-%d", nodeIndex)),
				}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
					Type: "SBRStorageUnhealthy", Status: status,
				}}}}
				watcher := watch.NewRaceFreeFake()
				recorder, err := helpers.StartObjectRecorder(helpers.RecorderConfig[*corev1.Node]{
					List: func(context.Context) ([]*corev1.Node, string, error) {
						return []*corev1.Node{node}, "1", nil
					},
					Watch: func(context.Context, string) (watch.Interface, error) { return watcher, nil },
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(recorder.Stop)
				caseRun.nodes = append(caseRun.nodes, &bpnNode{initial: node, recorder: recorder})
			}
			from := time.Now()
			err := caseRun.checkHealthyBoots(from)
			if (err != nil) != unhealthy {
				t.Fatalf("initial health error = %v, want error %t", err, unhealthy)
			}
		})
	}
}

func TestBPNPermanentCoverageUsesOnlyRequiredInterval(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	for _, example := range []struct {
		name             string
		gapStart, gapEnd time.Time
		wantPermanent    bool
	}{
		{"earlier history", start.Add(-time.Minute), start.Add(-time.Second), false},
		{"later history", start.Add(2 * time.Minute), start.Add(3 * time.Minute), false},
		{"intersecting history", start.Add(-time.Second), start.Add(time.Second), true},
	} {
		t.Run(example.name, func(t *testing.T) {
			snapshot := helpers.RecorderSnapshot[*corev1.Node]{
				PendingSince: start,
				Gaps:         []helpers.RecorderGap{{StartedAt: example.gapStart, EndedAt: example.gapEnd, Reason: "expired"}},
			}
			err := bpnPermanentCoverage(snapshot, start, start.Add(time.Minute))
			if bpnIsPermanent(err) != example.wantPermanent {
				t.Fatalf("permanent coverage failure = %v, want %t", err, example.wantPermanent)
			}
		})
	}
	snapshot := helpers.RecorderSnapshot[*corev1.Node]{PendingSince: start}
	if err := bpnPermanentCoverage(snapshot, start, time.Now()); err != nil {
		t.Fatalf("pending replay must remain retryable: %v", err)
	}
}

func TestBPNRetryResultStopsOnlyPermanentEvidence(t *testing.T) {
	transient := fmt.Errorf("temporary API failure")
	if !errors.Is(bpnRetryResult(transient), transient) || bpnRetryResult(nil) != nil {
		t.Fatal("transient errors and success must retain their normal polling behavior")
	}
	permanent := bpnPermanentError{fmt.Errorf("Node UID changed")}
	if result := bpnRetryResult(fmt.Errorf("Node check: %w", permanent)); result == nil ||
		!strings.Contains(result.Error(), "Required evidence cannot recover") {
		t.Fatalf("permanent wrapped evidence must stop retries: %v", result)
	}
}
