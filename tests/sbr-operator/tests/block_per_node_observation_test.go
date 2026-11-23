package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

func bpnNewSBRWithIncompleteCleanup() (*unstructured.Unstructured, *corev1.Node, time.Time) {
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", UID: "node-original"}}
	request := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": node.Name, "namespace": "operator", "uid": "request-original"},
		"status": map[string]interface{}{
			"policy": sbrparams.BPNPolicyOnTimeout, "beforeBootID": "original-boot",
			"offeredAt":      start.Add(-sbrparams.BPNRebootGrace).Format(time.RFC3339Nano),
			"rebootDeadline": start.Format(time.RFC3339Nano),
			"cleanup": map[string]interface{}{
				"startedAt":           start.Format(time.RFC3339Nano),
				"deadline":            start.Add(sbrparams.BPNCleanupPeriod).Format(time.RFC3339Nano),
				"phase":               "Incomplete",
				"outOfServiceApplied": true,
			},
			"conditions": []interface{}{
				map[string]interface{}{"type": "FencingSucceeded", "status": "True", "reason": bpnRebootGraceExpired},
				map[string]interface{}{"type": "Succeeded", "status": "True"},
				map[string]interface{}{"type": "Ready", "status": "True"},
				map[string]interface{}{"type": "Processing", "status": "False"},
				map[string]interface{}{"type": "FencingInProgress", "status": "False"},
			},
		},
	}}

	return request, node, start
}

func bpnSBRModifiedObservation(
	object *unstructured.Unstructured, at time.Time,
) helpers.RecorderObservation[*unstructured.Unstructured] {
	return helpers.RecorderObservation[*unstructured.Unstructured]{
		Object: object, At: at, UID: object.GetUID(), EventType: watch.Modified,
	}
}

func TestBPNTerminalRequestedResultRequiresCleanupPhase(t *testing.T) {
	for _, phase := range []string{"Completed", "Incomplete", "Unknown", "", "Pending", "Skipped"} {
		t.Run(phase, func(t *testing.T) {
			request, _, _ := bpnNewSBRWithIncompleteCleanup()
			if err := unstructured.SetNestedField(request.Object, phase, "status", "cleanup", "phase"); err != nil {
				t.Fatal(err)
			}
			err := bpnCheckSBRSuccessAndCleanupResult(request)
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
			request, node, _ := bpnNewSBRWithIncompleteCleanup()
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
				request, node, start := bpnNewSBRWithIncompleteCleanup()
				changed := request.DeepCopy()
				if action == "remove" {
					unstructured.RemoveNestedField(changed.Object, "status", "cleanup", field)
				} else if err := unstructured.SetNestedField(changed.Object,
					start.Add(time.Hour).Format(time.RFC3339Nano), "status", "cleanup", field); err != nil {
					t.Fatal(err)
				}
				samples := []helpers.RecorderObservation[*unstructured.Unstructured]{
					bpnSBRModifiedObservation(request, start), bpnSBRModifiedObservation(changed, start.Add(time.Second)),
					bpnSBRModifiedObservation(request.DeepCopy(), start.Add(2*time.Second)),
				}
				evidence := bpnCleanupEvidence{startedAt: start, deadline: start.Add(sbrparams.BPNCleanupPeriod)}
				if err := bpnCleanupHistory(samples, request, node, evidence); err == nil {
					t.Fatal("temporary timestamp change must fail even after restoration")
				}
			})
		}
	}
}

func TestBPNPolicyEvidenceCanPrecedeTerminalCleanupAndCRDeletion(t *testing.T) {
	request, node, start := bpnNewSBRWithIncompleteCleanup()
	if err := unstructured.SetNestedField(request.Object, "Observing", "status", "cleanup", "phase"); err != nil {
		t.Fatal(err)
	}
	publishedBy := start.Add(time.Minute)
	deleted := bpnSBRModifiedObservation(request, publishedBy)
	deleted.EventType = watch.Deleted
	samples := []helpers.RecorderObservation[*unstructured.Unstructured]{
		bpnSBRModifiedObservation(request, start), deleted,
	}
	evidence, err := bpnOOSPolicyEvidence(samples, request, node, start, publishedBy)
	if err != nil || !evidence.publishedAt.Equal(start) ||
		!evidence.deadline.Equal(start.Add(sbrparams.BPNCleanupPeriod)) {
		t.Fatalf("early success evidence must remain valid after CR deletion: %+v, %v", evidence, err)
	}
	if err := bpnCleanupHistory(samples, request, node, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := bpnOOSPolicyEvidence(samples[1:], request, node, start, publishedBy); err == nil {
		t.Fatal("a Delete event alone cannot prove active policy success")
	}

	late := []helpers.RecorderObservation[*unstructured.Unstructured]{
		bpnSBRModifiedObservation(request, publishedBy.Add(time.Second)),
	}
	if _, err := bpnOOSPolicyEvidence(late, request, node, start, publishedBy); err == nil {
		t.Fatal("a late observation cannot prove timely policy success")
	}
}

func TestBPNRetainedCleanupAllowsEarlyPolicySuccess(t *testing.T) {
	request, _, start := bpnNewSBRWithIncompleteCleanup()
	deadline := start.Add(sbrparams.BPNCleanupPeriod)
	if err := bpnRetainedCleanupResult(request, deadline.Add(-time.Second), deadline); err == nil {
		t.Fatal("a retained Pod must keep cleanup active until the stored deadline")
	}
	if err := bpnRetainedCleanupResult(request, deadline, deadline); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(request.Object, "Observing", "status", "cleanup", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := bpnRetainedCleanupResult(request, start, deadline); err != nil {
		t.Fatal("a retained Pod must permit policy success while cleanup continues", err)
	}
	if err := unstructured.SetNestedField(request.Object, "Completed", "status", "cleanup", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := bpnRetainedCleanupResult(request, deadline, deadline); err == nil {
		t.Fatal("a retained Pod must prevent cleanup Completed")
	}
}

func TestBPNObservationRejectsUnhealthyBaselineWithoutLaterEvents(t *testing.T) {
	for _, unhealthyIndex := range []int{-1, 0, 4} {
		t.Run(fmt.Sprintf("unhealthy worker index %d", unhealthyIndex), func(t *testing.T) {
			caseRun := &bpnFixture{}
			for nodeIndex := range 5 {
				status := corev1.ConditionFalse
				if nodeIndex == unhealthyIndex {
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
			if (err != nil) != (unhealthyIndex >= 0) {
				t.Fatalf("initial health error = %v, unhealthy worker index = %d", err, unhealthyIndex)
			}
		})
	}
}

func TestBPNPeerRestrictionsPermitOnlyTargetRemediation(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		peerRequest  int
		cordon       bool
		outOfService bool
	}{
		{name: "target remediation"},
		{name: "deleted B request", peerRequest: 1},
		{name: "deleted C request", peerRequest: 2},
		{name: "deleted request for fifth worker", peerRequest: 4},
		{name: "restored B cordon", cordon: true},
		{name: "removed C OutOfService", outOfService: true},
	} {
		from := time.Now()
		caseRun := &bpnFixture{}
		for index := range 5 {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("worker-%d", index), UID: types.UID(fmt.Sprintf("node-%d", index)),
			}}
			current := node.DeepCopy()
			current.Spec.Unschedulable = index == 0 || (index == 1 && scenario.cordon)
			if index == 0 || (index == 2 && scenario.outOfService) {
				current.Spec.Taints = []corev1.Taint{{
					Key: "node.kubernetes.io/out-of-service", Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute,
				}}
			}
			watcher := watch.NewRaceFreeFake()
			recorder, err := helpers.StartObjectRecorder(helpers.RecorderConfig[*corev1.Node]{
				List: func(context.Context) ([]*corev1.Node, string, error) {
					return []*corev1.Node{current}, "1", nil
				},
				Watch: func(context.Context, string) (watch.Interface, error) { return watcher, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(recorder.Stop)
			caseRun.nodes = append(caseRun.nodes, &bpnNode{initial: node, recorder: recorder})
			watcher.Modify(node)
		}
		request := &unstructured.Unstructured{}
		request.SetName(caseRun.nodes[scenario.peerRequest].initial.Name)
		request.SetUID("request-original")
		requestWatcher := watch.NewRaceFreeFake()
		recorder, err := helpers.StartObjectRecorder(helpers.RecorderConfig[*unstructured.Unstructured]{
			List: func(context.Context) ([]*unstructured.Unstructured, string, error) {
				return []*unstructured.Unstructured{request}, "1", nil
			},
			Watch: func(context.Context, string) (watch.Interface, error) { return requestWatcher, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(recorder.Stop)
		caseRun.crRecorder = recorder
		requestWatcher.Delete(request)
		err = wait.PollUntilContextTimeout(context.Background(), time.Millisecond, 5*time.Second, true,
			func(context.Context) (bool, error) {
				for _, node := range caseRun.nodes {
					if len(node.recorder.Snapshot().Observations) < 2 {
						return false, nil
					}
				}

				return len(recorder.Snapshot().Observations) >= 2, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		err = caseRun.checkNoSBRsOrRestrictionChanges(from, caseRun.peerIndices()...)
		wantFailure := scenario.peerRequest != 0 || scenario.cordon || scenario.outOfService
		if (err != nil) != wantFailure {
			t.Fatalf("%s: got %v, want failure %t", scenario.name, err, wantFailure)
		}
		if err := caseRun.checkNoSBRsOrRestrictionChanges(from); err == nil {
			t.Fatal("the default scope must still reject target requests and restrictions")
		}
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

func TestBPNCrashReplaysNHCRequestAfterDeletion(t *testing.T) {
	request, node, start := bpnNewSBRWithIncompleteCleanup()
	const nhcUID types.UID = "test-nhc"
	request.SetNamespace(medik8sparams.OperatorNs)
	request.SetOwnerReferences([]metav1.OwnerReference{{UID: nhcUID}})
	added := bpnSBRModifiedObservation(request, start.Add(time.Second))
	added.EventType = watch.Added
	deleted := bpnSBRModifiedObservation(request, start.Add(time.Minute))
	deleted.EventType = watch.Deleted
	samples := []helpers.RecorderObservation[*unstructured.Unstructured]{added, deleted}
	observed, at, err := bpnRecordedNHCOwnedSBRCR(samples, node, nhcUID, start, deleted.At)
	if err != nil || observed.GetUID() != request.GetUID() || !at.Equal(added.At) {
		t.Fatalf("a timely NHC request must remain observable after deletion: got %v at %v, err %v", observed, at, err)
	}
	if _, _, err := bpnRecordedNHCOwnedSBRCR(samples, node, "other-nhc", start, deleted.At); err == nil {
		t.Fatal("the request must belong to the test NHC")
	}
	if _, _, err := bpnRecordedNHCOwnedSBRCR(samples, node, nhcUID, start, start); err == nil {
		t.Fatal("the request must be observed before the deadline")
	}
	if _, _, err := bpnRecordedNHCOwnedSBRCR(samples, node, nhcUID, deleted.At, deleted.At); err == nil {
		t.Fatal("a Delete event alone must not prove the request was active during the interval")
	}
}

func TestBPNCrashRequestLifetimeRequiresOriginalUIDAndDeletion(t *testing.T) {
	request, node, start := bpnNewSBRWithIncompleteCleanup()
	baseline := start.Add(-time.Minute)
	added := bpnSBRModifiedObservation(request, start.Add(time.Second))
	added.EventType = watch.Added
	deleted := bpnSBRModifiedObservation(request, start.Add(time.Minute))
	deleted.EventType = watch.Deleted
	samples := []helpers.RecorderObservation[*unstructured.Unstructured]{added, deleted}
	if err := bpnCheckCrashRequestLifetime(samples, node, request, baseline, start); err != nil {
		t.Fatalf("the original request must be deleted: %v", err)
	}
	if err := bpnCheckCrashRequestLifetime(samples[:1], node, request, baseline, start); err == nil {
		t.Fatal("an original request with no Delete event must fail")
	}
	replacement := request.DeepCopy()
	replacement.SetUID("replacement")
	samples = append(samples, bpnSBRModifiedObservation(replacement, deleted.At.Add(time.Second)))
	if err := bpnCheckCrashRequestLifetime(samples, node, request, baseline, start); err == nil {
		t.Fatal("a replacement request must fail even after the original was deleted")
	}
}

func TestBPNCrashBootHistoryAllowsDelayedOriginalSample(t *testing.T) {
	node := &bpnNode{initial: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "target"}}, boot: "new-boot"}
	from := time.Now()
	samples := []bpnBootSample{
		{Node: "target", At: from, ID: "new-boot"},
		{Node: "target", At: from.Add(time.Second), ID: "original-boot"},
	}
	if err := bpnCheckTargetBootChange(samples, node, from, "original-boot"); err != nil {
		t.Fatalf("a delayed original boot observation is valid: %v", err)
	}
	samples = append(samples, bpnBootSample{Node: "target", At: from.Add(2 * time.Second), ID: "extra-boot"})
	if err := bpnCheckTargetBootChange(samples, node, from, "original-boot"); err == nil {
		t.Fatal("a third distinct target boot ID must fail")
	}
}
