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
		WithVolume(corev1.Volume{Name: "shared", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name, ReadOnly: true},
		}})
	reader.Definition.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "shared", MountPath: "/sbr-data", ReadOnly: true},
	}
	reader.Definition.Labels = map[string]string{sbrparams.NodeMapTestLabel: "true"}
	reader.Definition.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		RunAsNonRoot: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	Expect(APIClient.Create(ctx, reader.Definition)).To(Succeed())
	reader.Object = reader.Definition.DeepCopy()
	test.observer = reader
	DeferCleanup(func() {
		_, err := reader.DeleteAndWait(medik8sparams.DefaultTimeout)
		Expect(err).NotTo(HaveOccurred(), "The read-only observer pod must be removed")
	})
	Expect(reader.WaitUntilRunning(medik8sparams.DefaultTimeout)).To(Succeed())
}

func (test *nodeMapCleanupTest) expectOriginalClaim(ctx context.Context, assertion Gomega) {
	claim, err := APIClient.PersistentVolumeClaims(medik8sparams.OperatorNs).Get(ctx, test.claim.Name, metav1.GetOptions{})
	assertion.Expect(err).NotTo(HaveOccurred())
	assertion.Expect(claim.UID).To(Equal(test.claim.UID), "Recreation must not provision a fresh PVC")
	assertion.Expect(claim.Spec.VolumeName).To(Equal(test.claim.Spec.VolumeName), "The backing PV must stay the same")
	assertion.Expect(claim.DeletionTimestamp).To(BeNil())
}

func (test *nodeMapCleanupTest) deleteAndRetainClaim(ctx context.Context) {
	Expect(APIClient.Delete(ctx, test.config)).To(Succeed())
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
	Eventually(func(assertion Gomega) {
		config := buildSBRC(test.config.GetName(), nil)
		assertion.Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(config), config)).To(Succeed())
		assertion.Expect(config.GetUID()).To(Equal(test.config.GetUID()))
		assertion.Expect(config.GetDeletionTimestamp()).NotTo(BeNil())
		assertion.Expect(config.GetFinalizers()).To(ContainElement(sbrparams.NodeMapTestFinalizer))
		assertion.Expect(config.GetFinalizers()).NotTo(ContainElement(sbrparams.SBRCOperatorCleanupFinalizer))
	}, sbrparams.NodeMapCleanupTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
		"The operator must finish its own deletion finalizer before the test retains storage")

	// The deleting SBRC can no longer reconcile ensurePVC and reattach ownership.
	// Remove only its owner reference, then release our hold so normal GC removes
	// the DaemonSet and init job. The fixture never patches a PV or changes storage data.
	claim := test.claim.DeepCopy()
	Expect(APIClient.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
	Expect(claim.UID).To(Equal(test.claim.UID))
	if claim.Labels == nil {
		claim.Labels = make(map[string]string)
	}
	claim.Labels[sbrparams.NodeMapTestLabel] = "true"
	claim.OwnerReferences = slices.DeleteFunc(claim.OwnerReferences, func(owner metav1.OwnerReference) bool {
		return owner.UID == test.config.GetUID()
	})
	Expect(APIClient.Update(ctx, claim)).To(Succeed())
	Expect(test.releaseHold(ctx)).To(Succeed())
	By("Waiting for every agent pod, the DaemonSet, the SBRC and the owned init job to be fully gone")
	test.waitForConfigDeletion(ctx)
}

func (test *nodeMapCleanupTest) releaseHold(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		config := buildSBRC(test.config.GetName(), nil)
		if err := APIClient.Get(ctx, client.ObjectKeyFromObject(config), config); err != nil {
			return client.IgnoreNotFound(err)
		}
		if config.GetUID() != test.config.GetUID() {
			return fmt.Errorf("refusing to change a replacement SBRC %s", config.GetName())
		}
		if !slices.Contains(config.GetFinalizers(), sbrparams.NodeMapTestFinalizer) {
			return nil
		}
		config.SetFinalizers(slices.DeleteFunc(config.GetFinalizers(), func(finalizer string) bool {
			return finalizer == sbrparams.NodeMapTestFinalizer
		}))

		return APIClient.Update(ctx, config)
	})
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
		client.Preconditions{UID: ptr.To(test.config.GetUID())}))).To(Succeed())
	Expect(test.releaseHold(ctx)).To(Succeed())
	test.waitForConfigDeletion(ctx)
	jobs, err := APIClient.K8sClient.BatchV1().Jobs(medik8sparams.OperatorNs).List(ctx,
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=sbr-cleanup,sbrconfig=" + test.config.GetName()})
	Expect(err).NotTo(HaveOccurred())
	for idx := range jobs.Items {
		job := &jobs.Items[idx]
		Expect(client.IgnoreNotFound(APIClient.Delete(ctx, job,
			client.Preconditions{UID: ptr.To(job.UID)}))).To(Succeed())
	}
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
