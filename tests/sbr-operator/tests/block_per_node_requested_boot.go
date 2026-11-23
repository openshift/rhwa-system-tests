package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
)

func bpnCheckOriginalRequestBoot(request *unstructured.Unstructured, originalBoot string) error {
	before, _, err := unstructured.NestedString(request.Object, "status", "beforeBootID")
	if err != nil || before == "" || before != originalBoot {
		return fmt.Errorf("CR %s beforeBootID %q must match the original host boot ID %q",
			request.GetUID(), before, originalBoot)
	}

	return nil
}

func bpnCheckBootBeforeRequest(samples []bpnBootSample, node, originalBoot string, from, requestAt time.Time) error {
	for _, sample := range samples {
		if sample.Node == node && !sample.At.Before(from) && sample.At.Before(requestAt) &&
			sample.ID != "" && sample.ID != originalBoot {
			return fmt.Errorf("node %s reported boot ID %s before request creation at %s", node, sample.ID, requestAt)
		}
	}

	return nil
}

func (test *bpnFixture) assertBootUnchangedBeforeRequest(originalBoot string, from, requestAt time.Time) {
	GinkgoHelper()
	test.mu.Lock()
	err := bpnCheckBootBeforeRequest(test.samples, test.nodes[0].initial.Name, originalBoot, from, requestAt)
	test.mu.Unlock()
	Expect(err).NotTo(HaveOccurred(), "The target must retain its original boot ID until NHC creates the request")
}

func bpnRequestedBootEvidence(
	samples []helpers.RecorderObservation[*unstructured.Unstructured], request *unstructured.Unstructured,
	node *corev1.Node, originalBoot, observedBoot string, from, deadline time.Time,
) (time.Time, error) {
	for _, sample := range samples {
		if sample.UID != request.GetUID() || sample.At.Before(from) || sample.At.After(deadline) ||
			sample.EventType == watch.Deleted {
			continue
		}
		if err := bpnRequestIdentity(sample.Object, request, node); err != nil {
			return time.Time{}, err
		}
		before, _, err := unstructured.NestedString(sample.Object.Object, "status", "beforeBootID")
		if err != nil || (before != "" && before != originalBoot) {
			return time.Time{}, bpnPermanentError{bpnCheckOriginalRequestBoot(sample.Object, originalBoot)}
		}
		if bpnHasVerifiedBootEvidence(sample.Object, observedBoot) {
			nodeUID, _, err := unstructured.NestedString(sample.Object.Object, "status", "nodeUID")
			if err != nil || nodeUID != string(node.UID) {
				return time.Time{}, bpnPermanentError{fmt.Errorf("CR %s boot evidence must name Node UID %s",
					request.GetUID(), node.UID)}
			}

			return sample.At, nil
		}
	}

	return time.Time{}, fmt.Errorf("CR %s has no verified boot observation for host boot ID %s",
		request.GetUID(), observedBoot)
}

func (test *bpnFixture) waitForRequestedTargetBoot(
	request *unstructured.Unstructured, requestAt, historyFrom time.Time,
) time.Time {
	GinkgoHelper()
	originalBoot := test.nodes[0].boot
	observedBoot, observedAt := test.waitForTargetHostBootIDChange(requestAt)
	test.nodes[0].boot = observedBoot
	test.assertBootUnchangedBeforeRequest(originalBoot, historyFrom, requestAt)
	deadline := observedAt.Add(sbrparams.BPNRelease)
	var evidenceAt time.Time
	reason := "The original CR must report verified boot evidence that matches the host"
	test.waitForAPICheck(bpnReplayWait(deadline), reason, func(_ context.Context) error {
		var err error
		evidenceAt, err = bpnRequestedBootEvidence(test.crRecorder.Snapshot().Observations,
			request, test.nodes[0].initial, originalBoot, observedBoot, requestAt, deadline)

		return err
	})
	test.waitForCompleteAPIHistory(requestAt, evidenceAt, false)

	return observedAt
}
