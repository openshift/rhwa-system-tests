package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (test *bpnFixture) targetAgent(ctx context.Context) (*corev1.Pod, error) {
	pods, err := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{
			LabelSelector: "sbrconfig=" + test.name,
			FieldSelector: "spec.nodeName=" + test.nodes[0].initial.Name,
		})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == sbrparams.AgentContainerName {
				return pod.DeepCopy(), nil
			}
		}
	}

	return nil, fmt.Errorf("target agent Pod is absent")
}

func (test *bpnFixture) restartAgent() {
	GinkgoHelper()
	var original *corev1.Pod
	Expect(test.api(func(ctx context.Context) error {
		var err error
		original, err = test.targetAgent(ctx)

		return err
	})).To(Succeed())
	test.deleteAgent(original)
	test.waitAgentReplacement(original)
}

func (test *bpnFixture) deleteAgent(original *corev1.Pod) {
	uid := original.UID
	Expect(test.api(func(ctx context.Context) error {
		return APIClient.Delete(ctx, original, client.Preconditions{UID: &uid})
	})).To(Succeed())
}

func (test *bpnFixture) waitAgentReplacement(original *corev1.Pod) {
	uid := original.UID
	test.eventually(sbrparams.BPNAgentReady, "A new Ready agent must replace the Pod", func(ctx context.Context) error {
		pod, err := test.targetAgent(ctx)
		if err != nil {
			return err
		}
		if pod.UID == uid || !bpnPodReady(pod) {
			return fmt.Errorf("a different Ready agent UID is required")
		}
		old, err := APIClient.CoreV1Interface.Pods(original.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && old.UID == uid {
			return fmt.Errorf("original agent Pod still exists")
		}
		node, err := test.getNode(ctx, 0)
		if err != nil {
			return err
		}
		if bpnHealth(node) != corev1.ConditionFalse {
			return fmt.Errorf("replacement must restore False storage health")
		}

		return test.checkBoots(test.baseline)
	})
	test.finalBoots()
}

func (test *bpnFixture) requireAgentRunning(rebootObservedAt time.Time) {
	var last error
	timeout := sbrparams.BPNAgentReady
	if !rebootObservedAt.IsZero() {
		timeout = time.Until(rebootObservedAt.Add(sbrparams.BPNAgentReady))
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	err := wait.PollUntilContextTimeout(test.ctx, sbrparams.BPNPoll, timeout, true,
		func(ctx context.Context) (bool, error) {
			last = test.api(func(call context.Context) error {
				pod, err := test.targetAgent(call)
				if err != nil {
					return err
				}

				return bpnRunningAgent(pod)
			})

			if bpnIsPermanent(last) {
				return false, last
			}

			return last == nil, nil
		})
	if err != nil && !rebootObservedAt.IsZero() {
		csiBlocked := false
		checkErr := test.api(func(ctx context.Context) error {
			var checkErr error
			csiBlocked, checkErr = test.agentAttachmentBlocked(ctx)

			return checkErr
		})
		Expect(checkErr).To(Succeed(), "The target agent must expose its startup result through the API")
		if csiBlocked {
			Fail(fmt.Sprintf("INCONCLUSIVE: CSI attachment prevented the target agent from starting: %v", last))
		}
	}
	Expect(err).NotTo(HaveOccurred(), "The target agent must run before a valid request")
}

func (test *bpnFixture) agentAttachmentBlocked(ctx context.Context) (bool, error) {
	pod, err := test.targetAgent(ctx)
	if err != nil {
		return false, err
	}
	if pod.Status.Phase != corev1.PodPending {
		return false, nil
	}
	events, err := APIClient.CoreV1Interface.Events(pod.Namespace).List(ctx,
		metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(pod.UID)})
	if err != nil {
		return false, err
	}
	for _, event := range events.Items {
		if event.Reason == "FailedAttachVolume" {
			return true, nil
		}
	}

	return false, nil
}

func (test *bpnFixture) healthyUntilDeadline(start, deadline time.Time) time.Time {
	GinkgoHelper()
	var confirmed time.Time
	remaining := time.Until(deadline)
	if remaining <= 0 {
		Fail("INCONCLUSIVE: the fixed recovery window ended before health confirmation")
	}
	ctx, cancel := context.WithDeadline(test.ctx, deadline)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, sbrparams.BPNPoll, remaining, true, func(call context.Context) (bool, error) {
		if err := test.liveAPI(call, 0); err != nil {
			return false, nil
		}
		freshFalse := false
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			fresh := sample.UID == test.nodes[0].initial.UID && !sample.At.Before(start)
			if fresh && bpnHealth(sample.Object) == corev1.ConditionFalse {
				freshFalse = true
			}
		}
		if !freshFalse {
			return false, nil
		}
		confirmed = time.Now()

		return true, nil
	})
	if err != nil {
		Fail("INCONCLUSIVE: fresh healthy storage and API transport did not fit the fixed recovery window")
	}

	return confirmed
}

func (test *bpnFixture) healthyContinuous(duration time.Duration) {
	from := time.Now()
	check := func() error {
		ctx, cancel := context.WithTimeout(test.ctx, sbrparams.BPNAPICall)
		defer cancel()
		if err := test.liveAPI(ctx, 0); err != nil {
			return err
		}
		if err := test.healthHistory(corev1.ConditionFalse, from, time.Now(), 0); err != nil {
			return err
		}

		return test.checkBoots(from)
	}
	Consistently(check, duration, sbrparams.BPNPoll).Should(Succeed(), "Keep storage and API access healthy")
	test.coverage(from, time.Now(), false)
	Expect(check()).To(Succeed(), "Replayed events must preserve continuous healthy recovery")
	test.finalBoots()
}

func (test *bpnFixture) holdHealthyUntil(from, deadline time.Time) {
	ctx, cancel := context.WithDeadline(test.ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(sbrparams.BPNPoll)
	defer ticker.Stop()
	for {
		if err := test.healthHistory(corev1.ConditionFalse, from, time.Now(), 0); err != nil {
			Fail("INCONCLUSIVE: the short recovery interval was not continuously healthy: " + err.Error())
		}
		if time.Until(deadline) <= sbrparams.BPNPoll {
			test.waitUntil(deadline)

			return
		}
		if err := test.liveAPI(ctx, 0); err != nil {
			Fail("INCONCLUSIVE: API transport did not remain reachable during the short recovery interval: " + err.Error())
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (test *bpnFixture) readyNodes() {
	test.eventually(sbrparams.BPNNodeReady, "Nodes must be Ready with matching boot IDs", func(ctx context.Context) error {
		for index, node := range test.nodes {
			current, err := test.getNode(ctx, index)
			if err != nil {
				return err
			}
			ready := false
			for _, condition := range current.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			if !ready {
				return fmt.Errorf("node %s is not Ready", node.initial.Name)
			}
			if current.Status.NodeInfo.BootID != node.boot {
				return fmt.Errorf("node %s API boot ID must match actual host boot ID", node.initial.Name)
			}
		}

		return nil
	})
}

func (test *bpnFixture) peerContinuity(from, through time.Time) {
	GinkgoHelper()
	test.coverage(from, through, true)
	snapshot := test.podRecorder.Snapshot()
	for _, node := range test.nodes[1:] {
		Expect(bpnPeerAgentInterval(snapshot.Observations, test.name, node.initial.Name, from, through)).To(Succeed())
	}
}

func bpnPeerAgentInterval(observations []helpers.RecorderObservation[*corev1.Pod], configName, nodeName string,
	from, through time.Time,
) error {
	present := make(map[types.UID]*corev1.Pod)
	for _, sample := range observations {
		if sample.At.After(from) {
			continue
		}
		if sample.EventType == watch.Deleted || !bpnSelectedAgentPod(sample.Object, configName, nodeName) {
			delete(present, sample.UID)

			continue
		}
		present[sample.UID] = sample.Object
	}
	if len(present) != 1 {
		return fmt.Errorf("peer %s needs exactly one baseline agent, observed %d", nodeName, len(present))
	}
	var baseline *corev1.Pod
	for _, pod := range present {
		baseline = pod
	}
	if err := bpnRunningAgent(baseline); err != nil {
		return fmt.Errorf("peer %s needs a running baseline agent: %w", nodeName, err)
	}
	for _, sample := range observations {
		outside := !sample.At.After(from) || sample.At.After(through)
		if outside || !bpnSelectedAgentPod(sample.Object, configName, nodeName) {
			continue
		}
		if sample.UID != baseline.UID || sample.EventType == watch.Deleted {
			return fmt.Errorf("peer %s agent must retain its UID and remain present", nodeName)
		}
		if err := bpnRunningAgent(sample.Object); err != nil {
			return fmt.Errorf("peer %s agent must keep running: %w", nodeName, err)
		}
		if bpnAgentRestarts(sample.Object) != bpnAgentRestarts(baseline) {
			return fmt.Errorf("peer %s agent must not restart", nodeName)
		}
	}

	return nil
}

func bpnSelectedAgentPod(pod *corev1.Pod, configName, nodeName string) bool {
	if !bpnSelectedPod(pod, configName, nodeName) {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == sbrparams.AgentContainerName {
			return true
		}
	}

	return false
}

func bpnAgentRestarts(pod *corev1.Pod) int32 {
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == sbrparams.AgentContainerName {
			return container.RestartCount
		}
	}

	return -1
}

func bpnRunningAgent(pod *corev1.Pod) error {
	if pod == nil || pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("the agent Pod must be Running")
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == sbrparams.AgentContainerName && container.State.Running != nil {
			return nil
		}
	}

	return fmt.Errorf("the agent container must have a running status")
}

func bpnPodGoneBefore(observations []helpers.RecorderObservation[*corev1.Pod], original *corev1.Pod,
	from, deadline time.Time,
) bool {
	for _, sample := range observations {
		if sample.At.Before(from) || sample.At.After(deadline) {
			continue
		}
		if sample.UID == original.UID && sample.EventType == watch.Deleted {
			return true
		}
		if sample.Object != nil && sample.Object.Namespace == original.Namespace && sample.Object.Name == original.Name &&
			sample.UID != "" && sample.UID != original.UID && sample.EventType != watch.Deleted {
			return true
		}
	}

	return false
}

func (test *bpnFixture) healthHistory(status corev1.ConditionStatus, from, through time.Time, indices ...int) error {
	for _, index := range indices {
		node := test.nodes[index]
		err := bpnHealthInterval(node.recorder.Snapshot().Observations, node.initial.UID, status, from, through)
		if err != nil {
			return fmt.Errorf("node %s: %w", node.initial.Name, err)
		}
	}

	return nil
}

func bpnHealthInterval(observations []helpers.RecorderObservation[*corev1.Node], uid types.UID,
	status corev1.ConditionStatus, from, through time.Time,
) error {
	var baseline *corev1.Node
	for _, sample := range observations {
		if sample.UID != uid || sample.At.After(through) {
			continue
		}
		if !sample.At.After(from) {
			baseline = sample.Object

			continue
		}
		if baseline == nil || bpnHealth(baseline) != status {
			return fmt.Errorf("the interval needs a %s storage baseline", status)
		}
		if bpnHealth(sample.Object) != status {
			return fmt.Errorf("storage health became %s at %s", bpnHealth(sample.Object), sample.At)
		}
	}
	if baseline == nil || bpnHealth(baseline) != status {
		return fmt.Errorf("the interval needs a %s storage baseline", status)
	}

	return nil
}
