package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/nodes"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// The NMO+SNR conflict test (OCP-69928): a node under NMO maintenance carries the
// exclude-from-remediation label, and SNR's agent DaemonSet excludes labelled nodes via a
// required node affinity, so the agent pod must leave the node and stay off it until
// maintenance ends. This port follows the Python test_nmo_snr_conflict; the Polarion plan
// additionally creates a SelfNodeRemediation CR and watches SNR decline to remediate, and
// those steps are intentionally not ported. The spec requires both operators and skips on
// clusters where SNR is not installed (the e2e-nmo-aws CI job installs only NMO).
var _ = Describe(
	"NMO SNR Maintenance Conflict",
	Ordered,
	Serial,
	Label(labels.OperatorNMO), func() {
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

			By("Verifying NMO deployment is Ready")

			nmoDeployment, err := deployment.Pull(
				APIClient, nmoparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get NMO deployment")
			Expect(nmoDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"NMO deployment is not Ready")

			By("Verifying SNR is installed")

			if !isSNRCRDInstalled(ctx) {
				Skip("SelfNodeRemediationConfig CRD not found -- OCP-69928 requires SNR alongside NMO")
			}

			By("Verifying the SNR agent DaemonSet is deployed")

			agentDaemonSet := &appsv1.DaemonSet{}
			err = APIClient.Get(ctx, client.ObjectKey{
				Namespace: medik8sparams.OperatorNs,
				Name:      nmoparams.SNRAgentDaemonSetName,
			}, agentDaemonSet)
			Expect(err).ToNot(HaveOccurred(),
				"SNR is installed but its agent DaemonSet %s was not found in %s",
				nmoparams.SNRAgentDaemonSetName, medik8sparams.OperatorNs)

			By("Verifying enough schedulable, non-control-plane worker nodes exist")

			eligible, err := listSchedulableWorkers(ctx)
			Expect(err).ToNot(HaveOccurred(), "Failed to list schedulable worker nodes")

			if len(eligible) < nmoparams.MinWorkerNodesForMaintenance {
				Skip(fmt.Sprintf(
					"The SNR conflict test requires at least %d schedulable non-control-plane worker nodes, found %d",
					nmoparams.MinWorkerNodesForMaintenance, len(eligible)))
			}

			By("Selecting a schedulable worker node")

			targetNodeName = selectSchedulableWorker(ctx)
			nmCRName = fmt.Sprintf("node-maintenance-%s", targetNodeName)

			By(fmt.Sprintf("Selected worker node: %s", targetNodeName))

			By("Pre-cleaning any stale NodeMaintenance CR from a previous run")
			deleteNMBestEffort(ctx, nmCRName)

			By("Registering NodeMaintenance cleanup and node recovery")
			DeferCleanup(func() {
				deleteNMBestEffort(ctx, nmCRName)
				waitForNodeRecoveryBestEffort(ctx, targetNodeName)
			})
		})

		It("Verify the SNR agent does not run on a node under NMO maintenance",
			reportxml.ID("69928"),
			Label(
				labels.DisruptionDestructive,
				labels.TierAcceptance,
				labels.PlatformAny,
				labels.ComponentRemediation,
				labels.FrequencyWeekly,
			), func() {
				By("Verifying an SNR agent pod is running on the target node before maintenance")
				verifySNRAgentPodOnNode(targetNodeName, nmoparams.SNRAgentPodTimeout)

				By(fmt.Sprintf("Creating NodeMaintenance CR %q for node %s", nmCRName, targetNodeName))

				nodeMaintenance := newNodeMaintenanceWithReason(
					nmCRName, targetNodeName, nmoparams.SNRConflictReason)
				Expect(APIClient.Create(ctx, nodeMaintenance)).To(Succeed(),
					"Failed to create NodeMaintenance CR")

				By("Waiting for the node to fully enter maintenance")
				assertMaintenanceSucceeded(ctx, nmCRName, targetNodeName)

				By("Waiting for the SNR agent pod to leave the node under maintenance")
				Eventually(func() (int, error) {
					return countSNRAgentPodsOnNode(targetNodeName)
				}, nmoparams.SNRAgentPodEvictionTimeout, nmoparams.DefaultPollInterval).Should(BeZero(),
					"SNR agent pods should be removed from node %s under maintenance", targetNodeName)

				By("Verifying the node carries the exclude-from-remediation label")

				node, err := nodes.Pull(APIClient, targetNodeName)
				Expect(err).ToNot(HaveOccurred(), "Failed to get node %s", targetNodeName)
				Expect(node.Object.Labels).To(HaveKeyWithValue(nmoparams.ExcludeRemediationLabel, "true"),
					"node %s should carry %s=true while under maintenance",
					targetNodeName, nmoparams.ExcludeRemediationLabel)

				By("Verifying no SNR agent pod runs on the node while it is under maintenance")
				Consistently(func() (int, error) {
					count, err := countSNRAgentPodsOnNode(targetNodeName)
					if err != nil {
						// A transient API error must not fail the window: retry the poll.
						// A persistent error still fails once the window ends.
						return 0, TryAgainAfter(nmoparams.DefaultPollInterval).Wrap(err)
					}

					return count, nil
				}, nmoparams.SNRAgentPodAbsenceWindow, nmoparams.DefaultPollInterval).Should(BeZero(),
					"SNR agent pods must not run on node %s while it is under maintenance", targetNodeName)

				By(fmt.Sprintf("Deleting NodeMaintenance CR %q and waiting for removal", nmCRName))
				deleteAndWaitForNMCR(ctx, nmCRName, nmoparams.UncordonTimeout)

				By("Verifying the node is uncordoned and the drain taint removed")
				assertNodeCordonAndTaint(targetNodeName, false, nmoparams.UncordonTimeout)

				By("Verifying the exclude-from-remediation label was removed from the node")

				node, err = nodes.Pull(APIClient, targetNodeName)
				Expect(err).ToNot(HaveOccurred(), "Failed to get node %s", targetNodeName)
				Expect(node.Object.Labels).ToNot(HaveKey(nmoparams.ExcludeRemediationLabel),
					"node %s should not carry %s after maintenance ended",
					targetNodeName, nmoparams.ExcludeRemediationLabel)

				By("Verifying the SNR agent pod returns to the node after maintenance")
				verifySNRAgentPodOnNode(targetNodeName, nmoparams.SNRAgentPodTimeout)
			})
	})

// isSNRCRDInstalled reports whether SNR is installed by checking for its CRD. It fails
// closed on unexpected errors: an API failure must not be mistaken for "SNR absent" and
// silently skip the spec.
func isSNRCRDInstalled(ctx context.Context) bool {
	GinkgoHelper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := APIClient.Get(ctx, client.ObjectKey{Name: nmoparams.SNRCRDName}, crd)
	if err == nil {
		return true
	}

	if errors.IsNotFound(err) {
		return false
	}

	Fail(fmt.Sprintf("checking SNR CRD %s: %v", nmoparams.SNRCRDName, err))

	return false
}

// countSNRAgentPodsOnNode returns how many SNR agent DaemonSet pods are bound to nodeName,
// regardless of phase. Any pod object still on the node counts: the conflict contract is
// that the agent leaves the node entirely while it is under maintenance.
func countSNRAgentPodsOnNode(nodeName string) (int, error) {
	onNode, err := listSNRAgentPodsOnNode(nodeName)
	if err != nil {
		return 0, err
	}

	return len(onNode), nil
}

// listSNRAgentPodsOnNode returns the SNR agent DaemonSet pods bound to nodeName. Agent
// pods are selected by their pod labels, never by name prefix.
func listSNRAgentPodsOnNode(nodeName string) ([]*pod.Builder, error) {
	agentPods, err := pod.List(APIClient, medik8sparams.OperatorNs, metav1.ListOptions{
		LabelSelector: nmoparams.SNRAgentPodLabelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing SNR agent pods in %s: %w", medik8sparams.OperatorNs, err)
	}

	var onNode []*pod.Builder

	for _, agentPod := range agentPods {
		if agentPod.Object.Spec.NodeName == nodeName {
			onNode = append(onNode, agentPod)
		}
	}

	return onNode, nil
}

// verifySNRAgentPodOnNode waits until at least one SNR agent pod on nodeName is Running
// with all containers ready. It is used both for the baseline before maintenance and for
// the agent's return after maintenance ends.
func verifySNRAgentPodOnNode(nodeName string, timeout time.Duration) {
	GinkgoHelper()

	Eventually(func() (int, error) {
		onNode, err := listSNRAgentPodsOnNode(nodeName)
		if err != nil {
			return 0, err
		}

		return len(helpers.FilterRunningPods(onNode)), nil
	}, timeout, nmoparams.DefaultPollInterval).Should(BeNumerically(">", 0),
		"no SNR agent pod is Running with all containers ready on node %s", nodeName)
}
