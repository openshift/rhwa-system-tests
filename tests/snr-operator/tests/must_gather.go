package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/infrastructure"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/mustgather"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe(
	"SNR Must-Gather Diagnostics",
	Serial,
	Ordered,
	Label(labels.OperatorSNR), func() {
		It("Verify SNR must-gather collects diagnostic data",
			reportxml.ID("50774"),
			Label(
				labels.DisruptionNonDestructive,
				labels.TierAcceptance,
				labels.PlatformAny,
				labels.ComponentController,
				labels.FrequencyWeekly,
			), func() {
				By("Verifying SNR deployment is Ready")

				snrDeployment, err := deployment.Pull(
					APIClient, snrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
				Expect(err).ToNot(HaveOccurred(), "Failed to get SNR deployment")
				Expect(snrDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
					"SNR deployment is not Ready")

				By("Detecting cluster topology")

				infraConfig, infraErr := infrastructure.Pull(APIClient)
				Expect(infraErr).ToNot(HaveOccurred(), "Failed to pull infrastructure configuration")

				if infraConfig.Object.Status.ControlPlaneTopology == configv1.ExternalTopologyMode {
					Skip("Must-gather test not supported on HyperShift clusters. " +
						"Node collection via 'oc adm inspect nodes' fails due to HyperShift API limitations " +
						"(0 nodes collected).")
				}

				By("Resolving the RHWA must-gather image")

				mustGatherImage := mustgather.ResolveImage(
					mustgather.ImageEnvVar, mustgather.DefaultImage, GinkgoWriter.Printf)
				Expect(mustGatherImage).To(ContainSubstring(":"),
					"must-gather image %q should contain a tag separator", mustGatherImage)

				By("Creating artifact directory for must-gather output")

				destDir, mkdirErr := mustgather.CreateDestDir("snr-must-gather-", GinkgoT().TempDir())
				Expect(mkdirErr).ToNot(HaveOccurred(), "Failed to create must-gather output directory")

				By("Capturing cluster state before must-gather for validation")

				listCtx, listCancel := context.WithTimeout(context.Background(), medik8sparams.DefaultTimeout)
				defer listCancel()

				nodeList, err := APIClient.CoreV1Interface.Nodes().List(listCtx, metav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred(), "Failed to list cluster nodes")
				Expect(nodeList.Items).ToNot(BeEmpty(), "Cluster has no nodes")

				var nodeNames []string
				for i := range nodeList.Items {
					nodeNames = append(nodeNames, nodeList.Items[i].Name)
				}

				By("Waiting for the SNR agent DaemonSet to be fully available")

				waitForSNRAgentDaemonSetAvailable()

				By("Running oc adm must-gather")

				testStartTime := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), mustgather.DefaultContextTimeout)
				defer cancel()

				DeferCleanup(func() {
					By("Cleaning up leftover must-gather namespaces")
					mustgather.CleanupNamespaces(context.Background(), testStartTime,
						mustgather.DefaultCleanupTimeout, GinkgoWriter.Printf)
				})

				Expect(mustgather.Run(ctx, mustGatherImage, destDir, mustgather.Options{
					ImageInfoTimeout: mustgather.DefaultImageInfoTimeout,
					OCTimeout:        mustgather.DefaultOCTimeout,
					SaveCommandLog:   true,
					HomeFallback:     true,
				}, GinkgoWriter.Printf)).To(Succeed(), "oc adm must-gather failed")

				By("Collecting gathered file paths")

				collectedFiles, walkErr := mustgather.CollectRelativePaths(destDir)
				Expect(walkErr).ToNot(HaveOccurred(), "Failed to walk must-gather output directory")
				Expect(collectedFiles).ToNot(BeEmpty(), "No files collected by must-gather")

				if writeErr := os.WriteFile(filepath.Join(destDir, "collected-paths.txt"),
					[]byte(strings.Join(collectedFiles, "\n")+"\n"), 0o644); writeErr != nil {
					GinkgoWriter.Printf("Warning: failed to write collected-paths.txt: %v\n", writeErr)
				}

				By("Validating node YAMLs for all cluster nodes")

				for _, nodeName := range nodeNames {
					Expect(mustgather.HasMatchingFile(collectedFiles, "nodes/"+nodeName+".yaml")).To(BeTrue(),
						"must-gather should contain YAML for node %s", nodeName)
				}

				GinkgoWriter.Printf("Node YAMLs collected: %d/%d\n", len(nodeNames), len(nodeNames))

				By("Validating SNR CRD definitions are present")

				for _, crdName := range snrparams.SNRCRDNames {
					Expect(mustgather.HasMatchingFile(collectedFiles, crdName+".yaml")).To(BeTrue(),
						"must-gather should contain CRD definition for %s", crdName)
				}

				By("Validating SNR controller and agent pod data is collected")

				// Match by stable name prefixes under pods/. Exact pod name suffixes churn when
				// the DaemonSet rolls during must-gather (common under --randomize-all when
				// sibling specs create SelfNodeRemediation CRs).
				Expect(mustgather.HasMatchingFile(collectedFiles,
					"/pods/"+snrparams.OperatorDeploymentName+"-")).To(BeTrue(),
					"must-gather should contain data for an SNR controller pod (%s-*)",
					snrparams.OperatorDeploymentName)
				Expect(mustgather.HasMatchingFile(collectedFiles,
					"/pods/"+snrparams.AgentDaemonSetName+"-")).To(BeTrue(),
					"must-gather should contain data for an SNR agent DaemonSet pod (%s-*)",
					snrparams.AgentDaemonSetName)
			})
	})

// waitForSNRAgentDaemonSetAvailable waits until the SNR agent DaemonSet has every
// desired pod ready and updated, so must-gather is less likely to race mid-rollout
// pods (ContainerCreating / disappearing names).
func waitForSNRAgentDaemonSetAvailable() {
	GinkgoHelper()

	Eventually(func(assertion Gomega) {
		agentDS := &appsv1.DaemonSet{}
		assertion.Expect(APIClient.Get(context.Background(), client.ObjectKey{
			Name: snrparams.AgentDaemonSetName, Namespace: medik8sparams.OperatorNs,
		}, agentDS)).To(Succeed(), "Failed to get SNR agent DaemonSet %q", snrparams.AgentDaemonSetName)
		desired := agentDS.Status.DesiredNumberScheduled
		assertion.Expect(desired).To(BeNumerically(">", 0),
			"SNR agent DaemonSet %q has no desired pods", snrparams.AgentDaemonSetName)
		assertion.Expect(agentDS.Status.ObservedGeneration).To(Equal(agentDS.Generation),
			"SNR agent DaemonSet %q has not observed generation %d (observed %d)",
			snrparams.AgentDaemonSetName, agentDS.Generation, agentDS.Status.ObservedGeneration)
		assertion.Expect(agentDS.Status.UpdatedNumberScheduled).To(Equal(desired),
			"SNR agent DaemonSet %q UpdatedNumberScheduled=%d, want %d",
			snrparams.AgentDaemonSetName, agentDS.Status.UpdatedNumberScheduled, desired)
		assertion.Expect(agentDS.Status.NumberReady).To(Equal(desired),
			"SNR agent DaemonSet %q NumberReady=%d, want %d",
			snrparams.AgentDaemonSetName, agentDS.Status.NumberReady, desired)
		assertion.Expect(agentDS.Status.NumberAvailable).To(Equal(desired),
			"SNR agent DaemonSet %q NumberAvailable=%d, want %d",
			snrparams.AgentDaemonSetName, agentDS.Status.NumberAvailable, desired)
	}, medik8sparams.DefaultTimeout, snrparams.DefaultPollInterval).Should(Succeed(),
		"SNR agent DaemonSet %q did not become fully available", snrparams.AgentDaemonSetName)
}
