package tests

import (
	"strings"
	"testing"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestBPNWatcherProbeUsesRawReadOnlyClaim(t *testing.T) {
	writer := bpnWatcherTestPod("writer", "worker-a")
	writer.Spec.Containers[0].Name = sbrparams.AgentContainerName
	writer.Labels = map[string]string{"sbrconfig": "config"}
	writer.Spec.Volumes[0].Ephemeral = &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{},
	}
	watcher := bpnWatcherTestPod("arbitrary-pod-name", "worker-b")
	watcher.Spec.Volumes[0].PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "writer-probe", ReadOnly: true,
	}
	pods := []corev1.Pod{writer, watcher}
	agent, claim, err := bpnFindWriterProbe(pods, "config", "worker-a")
	if err != nil || agent.pod.UID != writer.UID || claim != "writer-probe" {
		t.Fatalf("discover the ephemeral probe from its writer: claim=%q, error=%v", claim, err)
	}
	match, err := bpnFindWatcherProbe(pods, agent.pod, claim, true)
	if err != nil || match.pod.UID != watcher.UID || match.devicePath != "/dev/probe" {
		t.Fatalf("discover an unlabelled raw Block watcher from the claim reference: %v", err)
	}

	for _, scenario := range []struct {
		name   string
		change func(*corev1.Pod)
	}{
		{name: "writable reference", change: func(pod *corev1.Pod) {
			pod.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly = false
		}},
		{name: "same node", change: func(pod *corev1.Pod) {
			pod.Spec.NodeName = writer.Spec.NodeName
		}},
		{name: "no raw block mapping", change: func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeDevices = nil
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			candidate := watcher.DeepCopy()
			scenario.change(candidate)
			_, err := bpnFindWatcherProbe([]corev1.Pod{writer, *candidate}, &writer, claim, true)
			if err == nil {
				t.Fatal("discovery must reject an unsafe watcher mapping")
			}
		})
	}
	second := watcher.DeepCopy()
	second.Name, second.UID = "second-watcher", "second-watcher-uid"
	_, err = bpnFindWatcherProbe(append(pods, *second), &writer, claim, true)
	if err == nil {
		t.Fatal("discovery must reject multiple consumers of the probe claim")
	}
}

func TestBPNWatcherProbeAllowsReadinessChangeDuringFault(t *testing.T) {
	writer := bpnWatcherTestPod("writer", "worker-a")
	watcher := bpnWatcherTestPod("watcher", "worker-b")
	watcher.Spec.Volumes[0].PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: "writer-probe", ReadOnly: true,
	}
	watcher.Status.Conditions[0].Status = corev1.ConditionFalse
	pods := []corev1.Pod{writer, watcher}
	if _, err := bpnFindWatcherProbe(pods, &writer, "writer-probe", true); err == nil {
		t.Fatal("the watcher must be Ready before injection")
	}
	if _, err := bpnFindWatcherProbe(pods, &writer, "writer-probe", false); err != nil {
		t.Fatalf("the running watcher can become unready during injection: %v", err)
	}
}

func TestBPNWatcherProbeRejectsReplacementBindings(t *testing.T) {
	pod := bpnWatcherTestPod("watcher", "worker-b")
	container := pod.Spec.Containers[0].Name
	containerID := strings.Repeat("a", 64)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: container, ContainerID: "cri-o://" + containerID,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	original := bpnProbeDevice{pod: &pod, devicePath: "/dev/probe", container: container, containerID: containerID}
	current := original
	current.pod = pod.DeepCopy()
	if !bpnSameProbeBinding(&original, &current) {
		t.Fatal("the original Pod and container must match")
	}

	current.pod.Status.ContainerStatuses[0].ContainerID = "cri-o://" + strings.Repeat("b", 64)
	if bpnSameProbeBinding(&original, &current) {
		t.Fatal("a restarted container must not match")
	}

	current.pod.Status.ContainerStatuses[0].ContainerID = "cri-o://" + containerID
	current.pod.UID = "replacement-pod"
	if bpnSameProbeBinding(&original, &current) {
		t.Fatal("a Pod with a replacement UID must not match")
	}
}

func bpnWatcherTestPod(name, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: medik8sparams.OperatorNs, Name: name, UID: types.UID(name + "-uid"),
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:          "probe-container",
				VolumeDevices: []corev1.VolumeDevice{{Name: "probe", DevicePath: "/dev/probe"}},
			}},
			Volumes: []corev1.Volume{{Name: "probe"}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}
