package tests

import (
	"context"
	"fmt"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"

	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (test *nodeMapCleanupTest) mountOriginalClaim(ctx context.Context) {
	daemonSet, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(ctx,
		sbrparams.SBRAgentDaemonSetPrefix+test.config.GetName(), metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	var claimName string
	for _, volume := range daemonSet.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			Expect(claimName).To(BeEmpty(), "The agents must use exactly one shared PVC")
			claimName = volume.PersistentVolumeClaim.ClaimName
		}
	}
	Expect(claimName).NotTo(BeEmpty())
	claim, err := APIClient.PersistentVolumeClaims(medik8sparams.OperatorNs).Get(ctx, claimName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(metav1.IsControlledBy(claim, test.config)).To(BeTrue())
	Expect(claim.Spec.VolumeName).NotTo(BeEmpty())
	if claim.Spec.VolumeMode != nil {
		Expect(*claim.Spec.VolumeMode).To(Equal(corev1.PersistentVolumeFilesystem))
	}
	test.claim = claim
	AddReportEntry("original-pvc", map[string]string{
		"name": claim.Name, "uid": string(claim.UID), "pv": claim.Spec.VolumeName,
	})
	reader := pod.NewBuilder(APIClient, "sbr-map-reader-"+string(test.config.GetUID())[:8],
		medik8sparams.OperatorNs, sbrparams.InjectorImage).
		WithCommand([]string{"sleep", "infinity"}).
		WithPrivilegedFlag().
		WithVolume(corev1.Volume{Name: "shared", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name, ReadOnly: true},
		}})
	reader.Definition.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "shared", MountPath: "/sbr-data", ReadOnly: true},
	}
	reader.Definition.Labels = map[string]string{sbrparams.NodeMapTestLabel: "true"}
	// SBR's files carry SELinux labels that restricted pods cannot read.
	// Reuse its existing SCC access, without host mounts or an API token.
	reader.Definition.Spec.ServiceAccountName = daemonSet.Spec.Template.Spec.ServiceAccountName
	reader.Definition.Spec.AutomountServiceAccountToken = ptr.To(false)
	Expect(APIClient.Create(ctx, reader.Definition)).To(Succeed())
	reader.Object = reader.Definition.DeepCopy()
	test.observer = reader
	DeferCleanup(func() {
		_, err := reader.DeleteAndWait(medik8sparams.DefaultTimeout)
		Expect(err).NotTo(HaveOccurred(), "The read-only observer pod must be removed")
	})
	Expect(reader.WaitUntilRunning(medik8sparams.DefaultTimeout)).To(Succeed())
	// A second, non-controller owner retains only the PVC. The SBRC and agents
	// follow normal deletion, without a test finalizer delaying their shutdown.
	claim = test.claim.DeepCopy()
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
			return err
		}
		if claim.UID != test.claim.UID {
			return fmt.Errorf("refusing to retain a replacement PVC %s", claim.Name)
		}
		if !slices.ContainsFunc(claim.OwnerReferences, func(owner metav1.OwnerReference) bool {
			return owner.UID == reader.Object.UID
		}) {
			claim.OwnerReferences = append(claim.OwnerReferences, metav1.OwnerReference{
				APIVersion: "v1", Kind: "Pod", Name: reader.Object.Name, UID: reader.Object.UID,
			})
		}

		return APIClient.Update(ctx, claim)
	})).To(Succeed(), "The observer must retain the original PVC without delaying SBRC deletion")
}

func (test *nodeMapCleanupTest) expectOriginalClaim(ctx context.Context, assertion Gomega) {
	claim, err := APIClient.PersistentVolumeClaims(medik8sparams.OperatorNs).Get(ctx, test.claim.Name, metav1.GetOptions{})
	assertion.Expect(err).NotTo(HaveOccurred())
	assertion.Expect(claim.UID).To(Equal(test.claim.UID), "Recreation must not provision a fresh PVC")
	assertion.Expect(claim.Spec.VolumeName).To(Equal(test.claim.Spec.VolumeName), "The backing PV must stay the same")
	assertion.Expect(claim.DeletionTimestamp).To(BeNil())
}

func (test *nodeMapCleanupTest) deleteAndRetainClaim(ctx context.Context) {
	Expect(APIClient.Delete(ctx, test.config,
		client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
	deleting := test.config.DeepCopy()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(deleting), deleting)).To(Succeed())
	Expect(deleting.GetDeletionTimestamp()).NotTo(BeNil())
	var completed *batchv1.Job
	Eventually(func(assertion Gomega) {
		jobs, err := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=sbr-cleanup,sbrconfig=" + test.config.GetName(),
		})
		assertion.Expect(err).NotTo(HaveOccurred(), "Cleanup Jobs for SBRC %s must be discoverable", test.config.GetName())
		for jobIdx := range jobs.Items {
			job := &jobs.Items[jobIdx]
			if job.CreationTimestamp.Before(deleting.GetDeletionTimestamp()) {
				continue
			}
			for _, condition := range job.Status.Conditions {
				if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
					completed = job.DeepCopy()
				}
			}
		}
		assertion.Expect(completed).NotTo(BeNil(), "A cleanup Job for this SBRC deletion must reach Complete")
	}, sbrparams.NodeMapCleanupTimeout, sbrparams.DefaultPollInterval).Should(Succeed())
	AddReportEntry("node-map-cleanup-job", map[string]interface{}{
		"name": completed.Name, "uid": completed.UID, "completed": completed.Status.CompletionTime,
	})
	By("Verifying cleanup completed while the original PVC is still available")
	test.expectOriginalClaim(ctx, Default)
	var mountsOriginalClaim bool
	for _, volume := range completed.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == test.claim.Name {
			mountsOriginalClaim = true
		}
	}
	Expect(mountsOriginalClaim).To(BeTrue(), "The successful cleanup Job must mount the original PVC")
	By("Waiting for every agent pod, the DaemonSet, the SBRC and the owned init job to be fully gone")
	test.waitForConfigDeletion(ctx)
	test.expectOriginalClaim(ctx, Default)
}

func (test *nodeMapCleanupTest) waitForConfigDeletion(ctx context.Context) {
	Expect(wait.PollUntilContextTimeout(ctx, sbrparams.DefaultPollInterval,
		sbrparams.NodeMapCleanupTimeout, true, func(ctx context.Context) (bool, error) {
			config := buildSBRC(test.config.GetName(), nil)
			err := APIClient.Get(ctx, client.ObjectKeyFromObject(config), config)
			if !apierrors.IsNotFound(err) {
				return false, err
			}
			_, err = APIClient.DaemonSets(medik8sparams.OperatorNs).Get(ctx,
				sbrparams.SBRAgentDaemonSetPrefix+test.config.GetName(), metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				return false, err
			}
			pods, err := APIClient.Pods(medik8sparams.OperatorNs).List(ctx,
				metav1.ListOptions{LabelSelector: "app=sbr-agent,sbrconfig=" + test.config.GetName()})
			if err != nil || len(pods.Items) != 0 {
				return false, err
			}
			jobs, err := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			for idx := range jobs.Items {
				if metav1.IsControlledBy(&jobs.Items[idx], test.config) {
					return false, nil
				}
			}

			return true, nil
		})).To(Succeed(), "The old SBRC, DaemonSet, all agent pods and owned init job must be gone")
}

func (test *nodeMapCleanupTest) cleanupConfig(ctx context.Context) {
	By("Cleaning up the test-owned SBRC and its agents")
	Expect(client.IgnoreNotFound(APIClient.Delete(ctx, test.config,
		client.Preconditions{UID: ptr.To(test.config.GetUID())},
		client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
	test.waitForConfigDeletion(ctx)
	selector := "app.kubernetes.io/component=sbr-cleanup,sbrconfig=" + test.config.GetName()
	jobs, err := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred())
	for idx := range jobs.Items {
		job := &jobs.Items[idx]
		Expect(client.IgnoreNotFound(APIClient.Delete(ctx, job,
			client.Preconditions{UID: ptr.To(job.UID)},
			client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
	}
	// Also remove pods orphaned by a previous interrupted cleanup. Even completed
	// pods can keep this claim protected while they still reference it.
	pods, err := APIClient.Pods(medik8sparams.OperatorNs).List(ctx, metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred())
	for idx := range pods.Items {
		cleanupPod := &pods.Items[idx]
		Expect(client.IgnoreNotFound(APIClient.Delete(ctx, cleanupPod,
			client.Preconditions{UID: ptr.To(cleanupPod.UID)}))).To(Succeed())
	}
	Expect(wait.PollUntilContextTimeout(ctx, sbrparams.DefaultPollInterval,
		sbrparams.NodeMapCleanupTimeout, true, func(ctx context.Context) (bool, error) {
			remaining, err := APIClient.Pods(medik8sparams.OperatorNs).List(ctx,
				metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return false, err
			}

			return len(remaining.Items) == 0, nil
		})).To(Succeed(), "Cleanup pods for SBRC %s must be gone before deleting its PVC", test.config.GetName())
}

func (test *nodeMapCleanupTest) cleanupClaim(ctx context.Context) {
	if test.claim == nil {
		return
	}
	By("Removing the PVC retained by this test")
	Expect(client.IgnoreNotFound(APIClient.Delete(ctx, test.claim,
		client.Preconditions{UID: ptr.To(test.claim.UID)}))).To(Succeed())
	Expect(wait.PollUntilContextTimeout(ctx, sbrparams.DefaultPollInterval,
		sbrparams.NodeMapCleanupTimeout, true, func(ctx context.Context) (bool, error) {
			_, err := APIClient.PersistentVolumeClaims(medik8sparams.OperatorNs).Get(
				ctx, test.claim.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}

			return false, err
		})).To(Succeed(), "The test-retained PVC must be fully removed")
}

// Only the test's own marker is eligible for recovery. Ordinary SBRCs, PVCs and
// operator Jobs are never swept namespace-wide, and operator finalizers are not stripped.
func cleanupStaleNodeMapResources(ctx context.Context) {
	selector := client.MatchingLabels{sbrparams.NodeMapTestLabel: "true"}
	observers := &corev1.PodList{}
	Expect(APIClient.List(ctx, observers, client.InNamespace(medik8sparams.OperatorNs), selector)).To(Succeed())
	for podIdx := range observers.Items {
		observer := &observers.Items[podIdx]
		Expect(client.IgnoreNotFound(APIClient.Delete(ctx, observer,
			client.Preconditions{UID: ptr.To(observer.UID)}))).To(Succeed())
		Expect(wait.PollUntilContextTimeout(ctx, sbrparams.DefaultPollInterval,
			medik8sparams.DefaultTimeout, true, func(ctx context.Context) (bool, error) {
				err := APIClient.Get(ctx, client.ObjectKeyFromObject(observer), &corev1.Pod{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}

				return false, err
			})).To(Succeed(), "An old node-map reader must be gone before a new run")
	}
	configs := medik8sparams.NewUnstructuredList(sbrparams.CRDGroup, sbrparams.CRDVersion,
		"StorageBasedRemediationConfigList")
	Expect(APIClient.List(ctx, configs, client.InNamespace(medik8sparams.OperatorNs), selector)).To(Succeed())
	for configIdx := range configs.Items {
		prior := &nodeMapCleanupTest{config: configs.Items[configIdx].DeepCopy()}
		claims, err := APIClient.PersistentVolumeClaims(medik8sparams.OperatorNs).List(ctx,
			metav1.ListOptions{LabelSelector: "sbrconfig=" + prior.config.GetName()})
		Expect(err).NotTo(HaveOccurred())
		prior.cleanupConfig(ctx)
		for claimIdx := range claims.Items {
			claim := &claims.Items[claimIdx]
			if metav1.IsControlledBy(claim, prior.config) || claim.Labels[sbrparams.NodeMapTestLabel] == "true" {
				prior.claim = claim
				prior.cleanupClaim(ctx)
			}
		}
	}
	claims := &corev1.PersistentVolumeClaimList{}
	Expect(APIClient.List(ctx, claims, client.InNamespace(medik8sparams.OperatorNs), selector)).To(Succeed())
	for claimIdx := range claims.Items {
		prior := &nodeMapCleanupTest{claim: &claims.Items[claimIdx]}
		prior.cleanupClaim(ctx)
	}
}
