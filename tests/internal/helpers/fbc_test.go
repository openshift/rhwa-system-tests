package helpers

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodContainerUsesDigestPinnedImageThroughTaggedSpec(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "manager", Image: "ttl.sh/operator:3h",
		}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", ImageID: "ttl.sh/operator@sha256:" + digest,
		}}},
	}

	if !podContainerUsesImage(pod, "manager", "ttl.sh/operator@sha256:"+digest) {
		t.Fatal("tagged container spec did not match its runtime digest")
	}

	if podContainerUsesImage(pod, "manager", "ttl.sh/operator@sha256:"+strings.Repeat("b", 64)) {
		t.Fatal("container matched a different runtime digest")
	}
}
