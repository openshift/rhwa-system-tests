package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool {
	return &b
}

// deployStressDaemonSet creates a stress-ng DaemonSet on all worker nodes.
func deployStressDaemonSet() []*pod.Builder {
	By("Creating stress-ng DaemonSet for load injection")

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbrparams.StressDaemonSetName,
			Namespace: medik8sparams.OperatorNs,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "sbr-perf-stressor",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "sbr-perf-stressor",
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"node-role.kubernetes.io/worker": "",
					},
					Containers: []corev1.Container{
						{
							Name:  "stress-ng",
							Image: sbrparams.StressNGImage,
							Command: []string{
								"/bin/bash",
								"-c",
								buildStressCommand(),
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("4"),
									corev1.ResourceMemory: resource.MustParse("8Gi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(true),
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
					// Use lower priority to avoid evicting critical pods
					PriorityClassName: "system-node-critical",
				},
			},
		},
	}

	// Remove priority class if it doesn't exist (not all clusters have it)
	ds.Spec.Template.Spec.PriorityClassName = ""

	err := APIClient.Create(context.TODO(), ds)
	Expect(err).ToNot(HaveOccurred(), "Failed to create stress DaemonSet")

	// Wait for pods to be scheduled
	Eventually(func() int {
		pods, _ := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: sbrparams.StressPodLabelSelector})
		return len(pods)
	}, time.Minute*2, time.Second*5).Should(BeNumerically(">=", 1),
		"Stress pods should be scheduled")

	// Return list of stress pods
	pods, err := pod.List(APIClient, medik8sparams.OperatorNs,
		metav1.ListOptions{LabelSelector: sbrparams.StressPodLabelSelector})
	Expect(err).ToNot(HaveOccurred(), "Failed to list stress pods")

	GinkgoWriter.Printf("Deployed stress DaemonSet with %d pods\n", len(pods))

	return pods
}

// buildStressCommand generates the stress-ng command line.
func buildStressCommand() string {
	return fmt.Sprintf(`
set -e
echo "Installing stress-ng..."
dnf install -y stress-ng 2>&1 || yum install -y stress-ng 2>&1

echo "Starting stress-ng with load profile:"
echo "  CPU: %d%% across 4 cores"
echo "  Memory: %d%% of available RAM"
echo "  I/O: 2 workers, 1GB files"

stress-ng \
  --cpu 4 --cpu-load %d \
  --vm 2 --vm-bytes %d%% \
  --hdd 2 --hdd-bytes 1G \
  --timeout 0 \
  --metrics-brief \
  --verbose

echo "stress-ng exited"
`,
		sbrparams.StressCPULoad,
		sbrparams.StressMemoryPercent,
		sbrparams.StressCPULoad,
		sbrparams.StressMemoryPercent,
	)
}

// countRunningPods returns the count of Running+Ready pods.
func countRunningPods(pods []*pod.Builder) int {
	running := helpers.FilterRunningPods(pods)
	return len(running)
}

// cleanupStressPods deletes the stress-ng DaemonSet and waits for pods to terminate.
func cleanupStressPods(pods []*pod.Builder) {
	if len(pods) == 0 {
		return
	}

	By("Removing stress-ng DaemonSet")

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbrparams.StressDaemonSetName,
			Namespace: medik8sparams.OperatorNs,
		},
	}

	err := APIClient.Delete(context.TODO(), ds)
	if err != nil {
		GinkgoWriter.Printf("Warning: failed to delete stress DaemonSet: %v\n", err)
	}

	// Wait for pods to terminate
	Eventually(func() int {
		pods, _ := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: sbrparams.StressPodLabelSelector})
		return len(pods)
	}, time.Minute*2, time.Second*5).Should(Equal(0),
		"Stress pods should be terminated")

	GinkgoWriter.Printf("Stress DaemonSet cleaned up\n")
}

// getAllAgentPods returns all agent pods from the current SBRC.
func getAllAgentPods() []*pod.Builder {
	pods, err := pod.List(APIClient, medik8sparams.OperatorNs,
		metav1.ListOptions{LabelSelector: sbrparams.AgentPodLabelSelector})
	Expect(err).ToNot(HaveOccurred(), "Failed to list agent pods")

	// Filter to running pods only
	running := helpers.FilterRunningPods(pods)

	return running
}

// waitForStressPodsRunning waits for all stress pods to reach Running state.
func waitForStressPodsRunning(expectedCount int) {
	By(fmt.Sprintf("Waiting for %d stress pods to reach Running state", expectedCount))

	Eventually(func() int {
		pods, err := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: sbrparams.StressPodLabelSelector})
		if err != nil {
			return 0
		}
		running := helpers.FilterRunningPods(pods)
		return len(running)
	}, time.Minute*3, time.Second*5).Should(Equal(expectedCount),
		"All stress pods should be Running+Ready")

	GinkgoWriter.Printf("All %d stress pods are Running+Ready\n", expectedCount)
}
