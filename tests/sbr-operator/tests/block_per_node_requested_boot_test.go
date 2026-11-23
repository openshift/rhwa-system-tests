package tests

import (
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBPNRequestedBootEvidenceMatchesOriginalRequestAndHost(t *testing.T) {
	for _, test := range []struct {
		name, phase, observation, before, after string
		wrongRequest, late, valid               bool
	}{
		{name: "accepted reboot", phase: "BootEnded", observation: "AcceptedBootEnded", valid: true},
		{name: "observed boot without acceptance", phase: "Cancelled", observation: "BootObserved", valid: true},
		{name: "matching IDs without verification"},
		{name: "wrong original boot", phase: "BootEnded", observation: "AcceptedBootEnded", before: "other-boot"},
		{name: "wrong observed boot", phase: "BootEnded", observation: "AcceptedBootEnded", after: "other-boot"},
		{name: "unrelated request", phase: "BootEnded", observation: "AcceptedBootEnded", wrongRequest: true},
		{name: "late evidence", phase: "BootEnded", observation: "AcceptedBootEnded", late: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, node, from := bpnNewSBRWithIncompleteCleanup()
			current := request.DeepCopy()
			previous, observed := "original-boot", "new-boot"
			if test.before != "" {
				previous = test.before
			}
			if test.after != "" {
				observed = test.after
			}
			if test.wrongRequest {
				current.SetUID("another-request")
			}
			status := map[string]interface{}{
				"nodeUID": string(node.UID), "beforeBootID": previous, "observedBootID": observed,
				"executionPhase": test.phase, "bootObservation": test.observation,
			}
			if err := unstructured.SetNestedMap(current.Object, status, "status"); err != nil {
				t.Fatal(err)
			}
			deadline := from.Add(time.Minute)
			observedAt := from.Add(time.Second)
			if test.late {
				observedAt = deadline.Add(time.Second)
			}
			samples := []helpers.RecorderObservation[*unstructured.Unstructured]{
				bpnSBRModifiedObservation(current, observedAt),
			}
			got, err := bpnRequestedBootEvidence(samples, request, node, "original-boot", "new-boot", from, deadline)
			if (err == nil) != test.valid || (test.valid && !got.Equal(observedAt)) {
				t.Fatalf("boot evidence time = %s, error = %v, want valid = %t", got, err, test.valid)
			}
		})
	}
}

func TestBPNRequestedBootRejectsPreRequestBootChange(t *testing.T) {
	from := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	requestAt := from.Add(time.Minute)
	for _, test := range []struct {
		name string
		at   time.Time
		node string
		want bool
	}{
		{name: "earlier cycle", at: from.Add(-time.Second), node: "target", want: true},
		{name: "other node", at: from, node: "peer", want: true},
		{name: "before request", at: requestAt.Add(-time.Second), node: "target"},
		{name: "after request", at: requestAt.Add(time.Second), node: "target", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			samples := []bpnBootSample{{At: test.at, Node: test.node, ID: "different-boot"}}
			err := bpnCheckBootBeforeRequest(samples, "target", "original-boot", from, requestAt)
			if (err == nil) != test.want {
				t.Fatalf("pre-request boot check error = %v, want valid = %t", err, test.want)
			}
		})
	}
}
