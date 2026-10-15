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
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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

	// Create ServiceAccount for privileged stress pods
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbr-perf-stressor",
			Namespace: medik8sparams.OperatorNs,
		},
	}
	saErr := APIClient.Create(context.TODO(), sa)
	if saErr != nil && !k8serrors.IsAlreadyExists(saErr) {
		Expect(saErr).ToNot(HaveOccurred(), "Failed to create stress ServiceAccount")
	}

	// Grant privileged SCC to the ServiceAccount
	sccBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sbr-perf-stressor-privileged",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:openshift:scc:privileged",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "sbr-perf-stressor",
				Namespace: medik8sparams.OperatorNs,
			},
		},
	}
	sccErr := APIClient.Create(context.TODO(), sccBinding)
	if sccErr != nil && !k8serrors.IsAlreadyExists(sccErr) {
		Expect(sccErr).ToNot(HaveOccurred(), "Failed to create privileged SCC binding")
	}

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
					ServiceAccountName: "sbr-perf-stressor",
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
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
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
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		Expect(err).ToNot(HaveOccurred(), "Failed to create stress DaemonSet")
	}
	if k8serrors.IsAlreadyExists(err) {
		GinkgoWriter.Printf("Stress DaemonSet already exists (leftover from previous run), continuing...\n")
	}

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

// buildStressCommand generates a bash-based stress command that doesn't require installation.
// Uses built-in utilities (dd, sha256sum) to generate CPU and I/O load.
func buildStressCommand() string {
	return `
set -e
echo "Starting load injection with bash stress generators..."
echo "  CPU: 4 workers doing continuous SHA256 hashing"
echo "  I/O: 2 workers doing continuous dd writes to /tmp"

# CPU stress: SHA256 hash computation in infinite loop
cpu_stress() {
    while true; do
        echo "cpu-stress-$$" | sha256sum > /dev/null
    done
}

# I/O stress: continuous writes with dd
io_stress() {
    while true; do
        dd if=/dev/zero of=/tmp/stress-io-$$ bs=1M count=100 2>/dev/null
        rm -f /tmp/stress-io-$$
    done
}

# Start CPU workers in background
for i in {1..4}; do
    cpu_stress &
done

# Start I/O workers in background
for i in {1..2}; do
    io_stress &
done

echo "Load injection started: 4 CPU workers + 2 I/O workers"

# Keep container alive
wait
`
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

	// Clean up ClusterRoleBinding
	sccBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sbr-perf-stressor-privileged",
		},
	}
	crbErr := APIClient.Delete(context.TODO(), sccBinding)
	if crbErr != nil && !k8serrors.IsNotFound(crbErr) {
		GinkgoWriter.Printf("Warning: failed to delete ClusterRoleBinding: %v\n", crbErr)
	}

	// Clean up ServiceAccount
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbr-perf-stressor",
			Namespace: medik8sparams.OperatorNs,
		},
	}
	saErr := APIClient.Delete(context.TODO(), sa)
	if saErr != nil && !k8serrors.IsNotFound(saErr) {
		GinkgoWriter.Printf("Warning: failed to delete ServiceAccount: %v\n", saErr)
	}

	GinkgoWriter.Printf("Stress DaemonSet and RBAC resources cleaned up\n")
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
func waitForStressPodsRunning(expectedCount int) int {
	By(fmt.Sprintf("Waiting for stress pods to reach Running state (need at least 1, target %d)", expectedCount))

	var runningCount int
	Eventually(func() int {
		pods, err := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: sbrparams.StressPodLabelSelector})
		if err != nil {
			return 0
		}
		running := helpers.FilterRunningPods(pods)
		runningCount = len(running)
		return runningCount
	}, time.Minute*3, time.Second*5).Should(BeNumerically(">=", 1),
		"At least one stress pod should be Running+Ready for load injection")

	GinkgoWriter.Printf("Stress load ready: %d/%d pods Running+Ready\n", runningCount, expectedCount)
	return runningCount
}
