package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// NodeMaintenance drain feedback test (RHWA-1252, OCP-50537). Ports the Python
// test_feedback_for_running_maintenance_mode: while a drain is running the NodeMaintenance
// CR must report the phase and the pods still pending eviction, and once the drain
// finishes it must report Succeeded with DrainProgress 100 and no PendingPods.
//
// The Python relies on whatever workload happens to run on the node to keep the drain
// observable, and silently accepts an already-completed drain. On an idle worker the
// drain finishes before any poll can see the Running phase, so this spec pins a
// one-replica Deployment to the target node and blocks its eviction with a
// PodDisruptionBudget: the NMO drainer evicts through the eviction API (v0.21.0
// internal/controller/nodemaintenance_controller.go, createDrainer sets no
// DisableEviction), which refuses to violate the budget, so the running state is
// guaranteed observable. Deleting the PDB lets the drain complete.
//
// Polarion-only checks -- pendingPods shrinking while drainProgress rises, the initial
// evictionPods equality, the 30s update cadence -- are intentionally not ported; the
// Python asserts none of them. The Python's is_nmo_drain_process_running also awaits
// .status.drainProgress existence; that is dropped too, because DrainProgress is an int
// with omitempty that legitimately stays 0 while the drain runs, so existence is not a
// reliable signal -- the completed state pins it to 100 exactly instead.
var _ = Describe("NMO Maintenance Drain Feedback",
	Serial,
	Ordered,
	ContinueOnFailure,
	Label(
		labels.OperatorNMO,
		nmoparams.Label,
		labels.TierAcceptance,
		labels.FrequencyWeekly,
	), func() {
		var (
			ctx            context.Context
			targetNodeName string
			nmCRName       string
		)

		BeforeAll(func() {
			ctx = context.Background()

			By("Registering NMO API scheme")
			Expect(APIClient.AttachScheme(nmov1beta1.AddToScheme)).To(Succeed(),
				"Failed to register NMO scheme")

			By("Verifying NMO operator deployment is ready")

			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NMO deployment")
			Expect(nmoDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NMO deployment is not Ready")

			By("Selecting a worker node that does not host the NMO controller")

			_, nmoPodNode := nmoControllerPod()
			eligible, err := listSchedulableWorkers(ctx)
			Expect(err).ToNot(HaveOccurred(), "Failed to list schedulable worker nodes")
			hasCandidate := false

			for _, node := range eligible {
				if node.Name != nmoPodNode {
					hasCandidate = true

					break
				}
			}

			if !hasCandidate {
				Skip(fmt.Sprintf(
					"Requires a schedulable non-control-plane worker node other than the NMO controller's node %s",
					nmoPodNode))
			}

			targetNodeName = selectSchedulableWorkerExcluding(ctx, nmoPodNode)
			nmCRName = nmoparams.FeedbackNMNamePrefix + targetNodeName
			GinkgoWriter.Printf("Target node for the drain feedback test: %s\n", targetNodeName)
		})

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() {
				GinkgoWriter.Println("Test failed - collecting NMO controller diagnostics")
				helpers.LogControllerState(ctx, APIClient,
					medik8sparams.OperatorNs, nmoparams.OperatorControllerPodLabels,
					GinkgoWriter.Printf)
			}
		})

		It("Report pending pods while the drain is running and succeed after it completes",
			reportxml.ID("50537"),
			Label(
				labels.DisruptionDestructive,
				labels.PlatformAny,
				labels.ComponentController,
			), func() {
				By("Creating the PDB-blocked workload on the target node")
				createFeedbackWorkload(ctx, targetNodeName)

				By("Waiting for the workload pod to run on the target node")
				blockedPodName := waitForFeedbackPodRunning(targetNodeName)

				By("Pre-cleaning a stale NodeMaintenance CR from a previous interrupted run")
				deleteNMBestEffort(ctx, nmCRName)

				By(fmt.Sprintf("Registering cleanup for NodeMaintenance %s and recovery of node %s",
					nmCRName, targetNodeName))
				DeferCleanup(func() {
					deleteNMBestEffort(ctx, nmCRName)
					waitForNodeRecoveryBestEffort(ctx, targetNodeName)
				})

				By(fmt.Sprintf("Creating NodeMaintenance %s for node %s", nmCRName, targetNodeName))

				nmCR := newNodeMaintenanceWithReason(nmCRName, targetNodeName, nmoparams.FeedbackReason)
				Expect(APIClient.Create(ctx, nmCR)).To(Succeed(),
					"Failed to create NodeMaintenance %s for node %s", nmCRName, targetNodeName)

				By("Waiting for the NodeMaintenance to report the running drain")
				Eventually(func() error {
					current := &nmov1beta1.NodeMaintenance{}
					if err := APIClient.Get(ctx, client.ObjectKey{Name: nmCRName}, current); err != nil {
						return fmt.Errorf("getting NodeMaintenance %s: %w", nmCRName, err)
					}

					if current.Status.Phase != nmov1beta1.MaintenanceRunning {
						return fmt.Errorf("phase is %q, want %q",
							current.Status.Phase, nmov1beta1.MaintenanceRunning)
					}

					for _, pendingPod := range current.Status.PendingPods {
						if pendingPod == blockedPodName {
							return nil
						}
					}

					return fmt.Errorf("pendingPods %v does not contain %s",
						current.Status.PendingPods, blockedPodName)
				}, nmoparams.MaintenanceTimeout, nmoparams.DefaultPollInterval).Should(Succeed(),
					"NodeMaintenance %s did not report the running drain with pod %s pending",
					nmCRName, blockedPodName)

				By("Deleting the PodDisruptionBudget to let the drain complete")
				Expect(APIClient.Delete(ctx, newFeedbackPdb())).To(Succeed(),
					"Failed to delete the drain feedback PodDisruptionBudget")

				By("Waiting for the NodeMaintenance to reach Succeeded phase")
				waitForMaintenanceSucceeded(ctx, nmCRName)

				By("Verifying the NodeMaintenance status shows the drain completed")
				assertDrainCompleted(ctx, nmCRName)
			})
	})

// newFeedbackDeployment builds the drain feedback workload Deployment: a single pod
// pinned to the node about to go under maintenance.
func newFeedbackDeployment(targetNodeName string) *appsv1.Deployment {
	workloadLabels := map[string]string{
		nmoparams.FeedbackPodLabelKey: nmoparams.FeedbackPodLabelValue,
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nmoparams.FeedbackDeploymentName,
			Namespace: medik8sparams.OperatorNs,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: workloadLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: workloadLabels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": targetNodeName,
					},
					Containers: []corev1.Container{
						{
							Name:    "sleep",
							Image:   nmoparams.WorkloadTestImage,
							Command: []string{"sleep", "3600"},
						},
					},
				},
			},
		},
	}
}

// newFeedbackPdb builds the PodDisruptionBudget that blocks the drain feedback workload's
// eviction. minAvailable=1 on the single replica pod allows no disruption, and the NMO
// drainer evicts through the eviction API, which refuses to violate the budget.
func newFeedbackPdb() *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nmoparams.FeedbackPdbName,
			Namespace: medik8sparams.OperatorNs,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: 1,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					nmoparams.FeedbackPodLabelKey: nmoparams.FeedbackPodLabelValue,
				},
			},
		},
	}
}

// createFeedbackWorkload creates the workload Deployment and its PodDisruptionBudget,
// waits until the budget is enforced, and registers the workload cleanup before any
// Create, so a failure between the two Creates cannot leak the Deployment. It
// pre-cleans stale objects from a previous interrupted run, so a leaked fixture cannot
// fail every later run with AlreadyExists.
func createFeedbackWorkload(ctx context.Context, targetNodeName string) {
	GinkgoHelper()

	By("Pre-cleaning stale drain feedback workload from a previous interrupted run")
	deleteFeedbackWorkload(ctx, targetNodeName)

	DeferCleanup(func() {
		By("Cleaning up the drain feedback workload")
		deleteFeedbackWorkload(ctx, targetNodeName)
	})

	By(fmt.Sprintf("Creating Deployment %s pinned to node %s",
		nmoparams.FeedbackDeploymentName, targetNodeName))
	feedbackDeployment := newFeedbackDeployment(targetNodeName)
	Expect(APIClient.Create(ctx, feedbackDeployment)).To(Succeed(),
		"Failed to create the drain feedback Deployment")

	By(fmt.Sprintf("Creating PodDisruptionBudget %s blocking the workload's eviction",
		nmoparams.FeedbackPdbName))
	feedbackPdb := newFeedbackPdb()
	Expect(APIClient.Create(ctx, feedbackPdb)).To(Succeed(),
		"Failed to create the drain feedback PodDisruptionBudget")

	By(fmt.Sprintf("Waiting for PodDisruptionBudget %s to be enforced",
		nmoparams.FeedbackPdbName))
	Eventually(func() error {
		current := &policyv1.PodDisruptionBudget{}
		if err := APIClient.Get(ctx, client.ObjectKey{
			Namespace: medik8sparams.OperatorNs,
			Name:      nmoparams.FeedbackPdbName,
		}, current); err != nil {
			return fmt.Errorf("getting PodDisruptionBudget %s: %w", nmoparams.FeedbackPdbName, err)
		}

		if current.Status.ObservedGeneration < current.Generation {
			return fmt.Errorf("PodDisruptionBudget %s status is not synced yet: observedGeneration %d < generation %d",
				nmoparams.FeedbackPdbName, current.Status.ObservedGeneration, current.Generation)
		}

		if current.Status.DisruptionsAllowed != 0 {
			return fmt.Errorf("PodDisruptionBudget %s allows %d disruptions, expected 0",
				nmoparams.FeedbackPdbName, current.Status.DisruptionsAllowed)
		}

		return nil
	}, medik8sparams.DefaultTimeout, nmoparams.DefaultPollInterval).Should(Succeed(),
		"PodDisruptionBudget %s should block the workload's eviction", nmoparams.FeedbackPdbName)
}

// deleteFeedbackWorkload deletes the drain feedback Deployment and PodDisruptionBudget
// and waits until both are gone, logging warnings instead of failing the spec.
func deleteFeedbackWorkload(ctx context.Context, targetNodeName string) {
	deleteObjectBestEffort(ctx, newFeedbackPdb(), "PodDisruptionBudget "+nmoparams.FeedbackPdbName)
	deleteObjectBestEffort(ctx, newFeedbackDeployment(targetNodeName),
		"Deployment "+nmoparams.FeedbackDeploymentName)
}

// waitForFeedbackPodRunning waits until exactly one workload pod runs container-ready on
// the target node and returns its name. The pod must be running before the
// NodeMaintenance is created, otherwise the drain has nothing to block on; requiring
// exactly one also rules out a stale pod from a previous run whose deletion has not
// been issued yet.
func waitForFeedbackPodRunning(targetNodeName string) string {
	GinkgoHelper()

	var blockedPodName string

	Eventually(func() error {
		allPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs,
			metav1.ListOptions{LabelSelector: nmoparams.FeedbackPodLabelSelector})
		if listErr != nil {
			return fmt.Errorf("listing drain feedback pods in %s: %w", medik8sparams.OperatorNs, listErr)
		}

		onTarget := make([]*pod.Builder, 0, len(allPods))

		for _, workloadPod := range allPods {
			if workloadPod.Object.Spec.NodeName == targetNodeName {
				onTarget = append(onTarget, workloadPod)
			}
		}

		running := helpers.FilterRunningPods(onTarget)
		if len(running) != 1 {
			podStates := make([]string, 0, len(onTarget))

			for _, workloadPod := range onTarget {
				podStates = append(podStates, fmt.Sprintf("%s (%s)",
					workloadPod.Object.Name, workloadPod.Object.Status.Phase))
			}

			return fmt.Errorf("expected exactly 1 running drain feedback workload pod on node %s, found %d: %v",
				targetNodeName, len(running), podStates)
		}

		blockedPodName = running[0].Object.Name

		return nil
	}, medik8sparams.DefaultTimeout, nmoparams.DefaultPollInterval).Should(Succeed(),
		"The drain feedback workload pod should run on node %s", targetNodeName)

	return blockedPodName
}
