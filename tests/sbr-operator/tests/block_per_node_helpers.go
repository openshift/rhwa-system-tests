package tests

import (
	"context"
	"errors"
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

func (test *bpnFixture) findTargetAgentPod(ctx context.Context) (*corev1.Pod, error) {
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

func (test *bpnFixture) deleteAgentPodByUID(original *corev1.Pod) {
	uid := original.UID
	Expect(test.callAPIWithNodeIdentityChecks(func(ctx context.Context) error {
		return APIClient.Delete(ctx, original, client.Preconditions{UID: &uid})
	})).To(Succeed())
}

func (test *bpnFixture) waitForReadyReplacementAndHealthyStorage(original *corev1.Pod) {
	uid := original.UID
	test.waitForAPICheck(sbrparams.BPNAgentReady, "A new Ready agent must replace the Pod",
		func(ctx context.Context) error {
			pod, err := test.findTargetAgentPod(ctx)
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
			if bpnStorageUnhealthyStatus(node) != corev1.ConditionFalse {
				return fmt.Errorf("replacement must restore False storage health")
			}

			return test.checkRecordedBootIDsUnchanged(test.baseline)
		})
	test.assertCurrentHostBootIDsMatch()
}

func (test *bpnFixture) waitForHealthyStorageAndAPIByDeadline(start, deadline time.Time) time.Time {
	GinkgoHelper()
	var confirmed time.Time
	remaining := time.Until(deadline)
	if remaining <= 0 {
		Fail("INCONCLUSIVE: the fixed recovery window ended before health confirmation")
	}
	ctx, cancel := context.WithDeadline(test.ctx, deadline)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, sbrparams.BPNPoll, remaining, true, func(call context.Context) (bool, error) {
		if err := test.probeAPIFromNodeNamespaces(call, 0); err != nil {
			return false, nil
		}
		freshFalse := false
		for _, sample := range test.nodes[0].recorder.Snapshot().Observations {
			fresh := sample.UID == test.nodes[0].initial.UID && !sample.At.Before(start)
			if fresh && bpnStorageUnhealthyStatus(sample.Object) == corev1.ConditionFalse {
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

func (test *bpnFixture) checkStorageHealthAndAPIAccessUntil(from, deadline time.Time) {
	ctx, cancel := context.WithDeadline(test.ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(sbrparams.BPNPoll)
	defer ticker.Stop()
	recoveryErr := errors.New("worker recovery has not been checked")
	for {
		if err := test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, from, time.Now(), 0); err != nil {
			Fail("INCONCLUSIVE: the short recovery interval was not continuously healthy: " + err.Error())
		}
		if time.Until(deadline) > sbrparams.BPNPoll {
			if err := test.probeAPIFromNodeNamespaces(ctx, 0); err != nil {
				Fail("INCONCLUSIVE: API transport did not remain reachable during the short recovery interval: " + err.Error())
			}
		}
		if recoveryErr != nil && ctx.Err() == nil {
			recoveryErr = test.checkWorkerRecoveryWithinPoll(ctx)
			if bpnIsPermanent(recoveryErr) {
				Fail(recoveryErr.Error())
			}
		}
		if time.Until(deadline) <= sbrparams.BPNPoll {
			test.waitUntil(deadline)

			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
	}
	Expect(recoveryErr).To(Succeed(), "All workers must recover within the fixed recovery window")
	Expect(test.checkRecordedNodeStorageHealth(corev1.ConditionFalse, from, deadline, 0)).To(Succeed(),
		"Storage health must remain False through the fixed recovery window")
}

func (test *bpnFixture) waitForReadyNodesWithExpectedBootIDs() {
	test.waitForAPICheck(sbrparams.BPNNodeReady, "Nodes must be Ready with matching boot IDs",
		func(ctx context.Context) error {
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

func (test *bpnFixture) assertPeerAgentsKeepRunningWithoutRestart(from, through time.Time) {
	GinkgoHelper()
	test.waitForCompleteAPIHistory(from, through, true)
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

func (test *bpnFixture) checkRecordedNodeStorageHealth(
	status corev1.ConditionStatus, from, through time.Time, indices ...int,
) error {
	if len(indices) == 0 {
		indices = test.workerIndices()
	}
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
		if baseline == nil || bpnStorageUnhealthyStatus(baseline) != status {
			return fmt.Errorf("the interval needs a %s storage baseline", status)
		}
		if bpnStorageUnhealthyStatus(sample.Object) != status {
			return fmt.Errorf("storage health became %s at %s", bpnStorageUnhealthyStatus(sample.Object), sample.At)
		}
	}
	if baseline == nil || bpnStorageUnhealthyStatus(baseline) != status {
		return fmt.Errorf("the interval needs a %s storage baseline", status)
	}

	return nil
}
