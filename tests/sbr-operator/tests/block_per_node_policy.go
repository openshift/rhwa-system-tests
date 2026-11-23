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

const bpnRebootGraceExpired = "RebootGraceExpired"

func bpnCheckOOSPolicySuccess(request *unstructured.Unstructured) error {
	applied, _, err := unstructured.NestedBool(request.Object, "status", "cleanup", "outOfServiceApplied")
	if err != nil || !applied {
		return fmt.Errorf("CR %s must confirm cleanup.outOfServiceApplied=true", request.GetUID())
	}

	for _, expected := range []struct{ name, status string }{
		{"FencingSucceeded", "True"},
		{"Succeeded", "True"},
		{"Ready", "True"},
		{"Processing", "False"},
		{"FencingInProgress", "False"},
	} {
		if !bpnCondition(request, expected.name, expected.status) {
			return fmt.Errorf("CR %s must report %s=%s", request.GetUID(), expected.name, expected.status)
		}
	}

	reason, _ := bpnConditionReason(request, "FencingSucceeded", "True")
	if reason == "" {
		return fmt.Errorf("CR %s must report a fencing success reason", request.GetUID())
	}
	if reason == bpnRebootGraceExpired {
		beforeBootID, _, _ := unstructured.NestedString(request.Object, "status", "beforeBootID")
		rebootDeadline, err := bpnRequestRebootDeadline(request, sbrparams.BPNPolicyOnTimeout, beforeBootID)
		if err != nil {
			return err
		}
		startedAt, _, err := bpnCleanupTimes(request)
		if err != nil {
			return err
		}
		if startedAt.Before(rebootDeadline) {
			return fmt.Errorf("CR %s confirmed OutOfService before reboot grace expiry", request.GetUID())
		}
	} else {
		observedBootID, _, _ := unstructured.NestedString(request.Object, "status", "observedBootID")
		if !bpnHasVerifiedBootEvidence(request, observedBootID) {
			return fmt.Errorf("CR %s reports fencing success without qualifying boot evidence", request.GetUID())
		}
	}

	return nil
}

func bpnOOSPolicyEvidence(
	samples []helpers.RecorderObservation[*unstructured.Unstructured], request *unstructured.Unstructured,
	node *corev1.Node, from, publishedBy time.Time,
) (bpnCleanupEvidence, error) {
	for _, sample := range samples {
		if sample.UID != request.GetUID() || sample.At.Before(from) || sample.At.After(publishedBy) ||
			sample.EventType == watch.Deleted || sample.Object.GetDeletionTimestamp() != nil {
			continue
		}
		if err := bpnRequestIdentity(sample.Object, request, node); err != nil {
			return bpnCleanupEvidence{}, err
		}
		if !bpnCondition(sample.Object, "FencingSucceeded", "True") {
			continue
		}
		if err := bpnCheckOOSPolicySuccess(sample.Object); err != nil {
			return bpnCleanupEvidence{}, bpnPermanentError{err}
		}

		start, deadline, err := bpnCleanupTimes(sample.Object)
		if err != nil {
			return bpnCleanupEvidence{}, err
		}
		if start.After(sample.At) {
			return bpnCleanupEvidence{}, fmt.Errorf("cleanup.startedAt follows the policy success observation")
		}

		return bpnCleanupEvidence{startedAt: start, deadline: deadline, publishedAt: sample.At, result: sample.Object}, nil
	}

	return bpnCleanupEvidence{}, fmt.Errorf("no timely OutOfService policy success for CR UID %s", request.GetUID())
}

func (test *bpnFixture) waitForOOSPolicySuccess(
	request *unstructured.Unstructured, from time.Time,
) bpnCleanupEvidence {
	return test.waitForOOSPolicySuccessBy(request, from, from.Add(sbrparams.BPNRelease))
}

func (test *bpnFixture) waitForOOSPolicySuccessBy(
	request *unstructured.Unstructured, from, publishedBy time.Time,
) bpnCleanupEvidence {
	GinkgoHelper()
	var evidence bpnCleanupEvidence
	test.waitForAPICheck(bpnReplayWait(publishedBy), "The original CR must report success after confirmed OutOfService",
		func(_ context.Context) error {
			var err error
			evidence, err = bpnOOSPolicyEvidence(test.crRecorder.Snapshot().Observations,
				request, test.nodes[0].initial, from, publishedBy)
			if err != nil {
				return err
			}
			configUID, _, err := unstructured.NestedString(evidence.result.Object, "status", "configUID")
			if err != nil || configUID != string(test.config.GetUID()) {
				return bpnPermanentError{fmt.Errorf("successful CR %s has configuration UID %q, expected %s",
					request.GetUID(), configUID, test.config.GetUID())}
			}
			nodeUID, _, err := unstructured.NestedString(evidence.result.Object, "status", "nodeUID")
			if err != nil || nodeUID != string(test.nodes[0].initial.UID) {
				return bpnPermanentError{fmt.Errorf("successful CR %s has Node UID %q, expected %s",
					request.GetUID(), nodeUID, test.nodes[0].initial.UID)}
			}

			return nil
		})
	taintFrom := from
	if test.adminAdded {
		taintFrom = test.baseline
	}
	test.waitForRecordedOutOfServiceTaint(taintFrom, publishedBy)
	test.waitForCompleteAPIHistory(from, evidence.publishedAt, false)
	Expect(bpnCleanupHistory(test.crRecorder.Snapshot().Observations,
		request, test.nodes[0].initial, evidence)).To(Succeed())

	return evidence
}

func (test *bpnFixture) waitForTerminalCleanup(request *unstructured.Unstructured, evidence bpnCleanupEvidence) {
	GinkgoHelper()
	publishedBy := evidence.deadline.Add(sbrparams.BPNPublish)
	test.waitForAPICheck(bpnReplayWait(publishedBy), "Cleanup must reach a terminal phase by its deadline plus PUBLISH",
		func(_ context.Context) error {
			samples := test.crRecorder.Snapshot().Observations
			if err := bpnCleanupHistory(samples, request, test.nodes[0].initial, evidence); err != nil {
				return bpnPermanentError{err}
			}
			for _, sample := range samples {
				if sample.UID == request.GetUID() && !sample.At.Before(evidence.publishedAt) &&
					!sample.At.After(publishedBy) && sample.EventType != watch.Deleted &&
					bpnCheckSBRSuccessAndCleanupResult(sample.Object) == nil {
					return nil
				}
			}

			return fmt.Errorf("CR UID %s has no timely terminal cleanup", request.GetUID())
		})
}

func bpnRequestRebootDeadline(object *unstructured.Unstructured, policy, beforeBootID string) (time.Time, error) {
	currentPolicy, _, _ := unstructured.NestedString(object.Object, "status", "policy")
	bootID, _, _ := unstructured.NestedString(object.Object, "status", "beforeBootID")
	if currentPolicy != policy || bootID == "" || bootID != beforeBootID {
		return time.Time{}, fmt.Errorf("CR must name policy %s and original boot ID %s, got %q and %q",
			policy, beforeBootID, currentPolicy, bootID)
	}

	offered, _, _ := unstructured.NestedString(object.Object, "status", "offeredAt")
	expires, _, _ := unstructured.NestedString(object.Object, "status", "rebootDeadline")
	offeredAt, offerErr := time.Parse(time.RFC3339Nano, offered)
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, expires)
	if offerErr != nil || deadlineErr != nil || offeredAt.IsZero() ||
		!deadline.Equal(offeredAt.Add(sbrparams.BPNRebootGrace)) {
		return time.Time{}, fmt.Errorf("CR reboot deadline %q must equal offer time %q plus %s",
			expires, offered, sbrparams.BPNRebootGrace)
	}

	return deadline, nil
}

func bpnHasVerifiedBootEvidence(request *unstructured.Unstructured, observedBootID string) bool {
	observation, _, _ := unstructured.NestedString(request.Object, "status", "bootObservation")
	phase, _, _ := unstructured.NestedString(request.Object, "status", "executionPhase")
	before, _, _ := unstructured.NestedString(request.Object, "status", "beforeBootID")
	after, _, _ := unstructured.NestedString(request.Object, "status", "observedBootID")

	return before != "" && after != "" && before != after && after == observedBootID &&
		((observation == "AcceptedBootEnded" && phase == "BootEnded") || observation == "BootObserved")
}

func (test *bpnFixture) waitForOnTimeoutWithoutBootEvidence(
	request *unstructured.Unstructured, from time.Time, originalBootID string,
) bpnCleanupEvidence {
	GinkgoHelper()
	evidence := test.waitForOOSPolicySuccess(request, from)
	deadline, err := bpnRequestRebootDeadline(evidence.result, sbrparams.BPNPolicyOnTimeout, originalBootID)
	Expect(err).NotTo(HaveOccurred())
	Expect(evidence.startedAt).To(BeTemporally(">=", deadline), "OnTimeout must wait for the saved reboot deadline")
	reason, _ := bpnConditionReason(evidence.result, "FencingSucceeded", "True")
	Expect(reason).To(Equal(bpnRebootGraceExpired))
	for _, sample := range test.crRecorder.Snapshot().Observations {
		if sample.UID == request.GetUID() && !sample.At.After(evidence.publishedAt) {
			observation, _, _ := unstructured.NestedString(sample.Object.Object, "status", "bootObservation")
			phase, _, _ := unstructured.NestedString(sample.Object.Object, "status", "executionPhase")
			Expect(observation).NotTo(Or(Equal("AcceptedBootEnded"), Equal("BootObserved")))
			Expect(phase).NotTo(Equal("BootEnded"))
		}
	}
	for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
		if sample.UID == test.nodes[0].initial.UID && !sample.At.Before(from) && sample.At.Before(deadline) {
			Expect(bpnOutOfService(sample.Object)).To(Equal(bpnOutOfService(test.nodes[0].initial)),
				"OnTimeout must preserve the baseline taint until the reboot deadline")
		}
	}

	return evidence
}

func (test *bpnFixture) waitPastRebootDeadlineWithoutOOS(
	request *unstructured.Unstructured, from time.Time, originalBootID string,
) {
	GinkgoHelper()
	var deadline time.Time
	test.waitForAPICheck(sbrparams.BPNRequestCreate, "OnReboot must save the request's reboot deadline",
		func(ctx context.Context) error {
			current, err := test.getAndCheckSBRCR(ctx, request)
			if err != nil {
				return err
			}
			deadline, err = bpnRequestRebootDeadline(current, sbrparams.BPNPolicyOnReboot, originalBootID)

			return err
		})
	through := deadline.Add(sbrparams.BPNPublish)
	if now := time.Now(); now.After(through) {
		through = now
	}
	test.waitUntil(through)
	test.waitForCompleteAPIHistory(from, through, false)
	for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
		if sample.UID == test.nodes[0].initial.UID && !sample.At.Before(from) && !sample.At.After(through) {
			Expect(bpnOutOfService(sample.Object)).To(Equal(bpnOutOfService(test.nodes[0].initial)),
				"OnReboot must preserve the baseline taint through reboot deadline expiry")
		}
	}
	for _, sample := range test.crRecorder.Snapshot().Observations {
		if sample.UID == request.GetUID() && !sample.At.Before(from) && !sample.At.After(through) {
			Expect(bpnCondition(sample.Object, "FencingSucceeded", "True")).To(BeFalse())
			Expect(bpnCondition(sample.Object, "Succeeded", "True")).To(BeFalse())
			applied, _, err := unstructured.NestedBool(sample.Object.Object, "status", "cleanup", "outOfServiceApplied")
			Expect(err).NotTo(HaveOccurred())
			Expect(applied).To(BeFalse(), "Reboot deadline expiry must leave OnReboot pending without boot evidence")
			observedBootID, _, _ := unstructured.NestedString(sample.Object.Object, "status", "observedBootID")
			Expect(bpnHasVerifiedBootEvidence(sample.Object, observedBootID)).To(BeFalse(),
				"The isolated target must not publish verified boot evidence")
		}
	}

	Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		current, err := test.getAndCheckSBRCR(ctx, request)
		if err == nil && current.GetDeletionTimestamp() != nil {
			return fmt.Errorf("the OnReboot CR entered deletion before the boot evidence test")
		}

		return err
	})).To(Succeed(), "The same OnReboot request must remain active after its reboot deadline")
}

func (test *bpnFixture) waitForOnRebootPolicySuccess(
	request *unstructured.Unstructured, from, apiRestoreAt time.Time, originalBootID, observedBootID string,
) bpnCleanupEvidence {
	GinkgoHelper()
	evidence := test.waitForOOSPolicySuccessBy(request, from, apiRestoreAt.Add(sbrparams.BPNRelease))
	Expect(evidence.publishedAt).To(BeTemporally(">=", apiRestoreAt), "OnReboot success must follow API restoration")
	_, err := bpnRequestRebootDeadline(evidence.result, sbrparams.BPNPolicyOnReboot, originalBootID)
	Expect(err).NotTo(HaveOccurred())
	Expect(bpnHasVerifiedBootEvidence(evidence.result, observedBootID)).To(BeTrue(),
		"OnReboot requires verified boot evidence for the same original request")
	reason, _ := bpnConditionReason(evidence.result, "FencingSucceeded", "True")
	Expect(reason).NotTo(Equal(bpnRebootGraceExpired))

	return evidence
}
