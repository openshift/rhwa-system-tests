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
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/mustgather"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

				mustGatherImage := mustgather.DiscoverImage(
					APIClient, medik8sparams.OperatorNs,
					snrparams.MustGatherImageEnvVar, snrparams.DefaultMustGatherImage, GinkgoWriter.Printf)
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

				snrPodNames := collectSNRPodNames()

				By("Running oc adm must-gather")

				testStartTime := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), snrparams.MustGatherContextTimeout)
				defer cancel()

				DeferCleanup(func() {
					By("Cleaning up leftover must-gather namespaces")
					mustgather.CleanupNamespaces(context.Background(), testStartTime,
						snrparams.MustGatherCleanupTimeout, GinkgoWriter.Printf)
				})

				Expect(mustgather.Run(ctx, mustGatherImage, destDir, mustgather.Options{
					ImageInfoTimeout: snrparams.MustGatherImageInfoTimeout,
					OCTimeout:        snrparams.MustGatherOCTimeout,
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

				for _, podName := range snrPodNames {
					Expect(mustgather.HasMatchingFile(collectedFiles, podName)).To(BeTrue(),
						"must-gather should contain data for SNR pod %s", podName)
				}

				GinkgoWriter.Printf("SNR pod data collected: %d/%d\n", len(snrPodNames), len(snrPodNames))
			})
	})

// collectSNRPodNames returns the names of the SNR controller-manager and agent DaemonSet pods
// that must-gather is expected to collect data for.
func collectSNRPodNames() []string {
	var podNames []string

	ctrlPods, ctrlErr := pod.List(APIClient, medik8sparams.OperatorNs, metav1.ListOptions{
		LabelSelector: snrparams.OperatorControllerPodLabelSelector,
	})
	Expect(ctrlErr).ToNot(HaveOccurred(), "Failed to list SNR controller pods")
	Expect(ctrlPods).ToNot(BeEmpty(), "No SNR controller pods found")
	dsPods, dsErr := pod.List(APIClient, medik8sparams.OperatorNs, metav1.ListOptions{
		LabelSelector: snrparams.DaemonSetPodLabelSelector,
	})
	Expect(dsErr).ToNot(HaveOccurred(), "Failed to list SNR DaemonSet pods")
	Expect(dsPods).ToNot(BeEmpty(), "No SNR DaemonSet pods found")

	for _, p := range ctrlPods {
		podNames = append(podNames, p.Object.Name)
	}

	for _, p := range dsPods {
		podNames = append(podNames, p.Object.Name)
	}

	return podNames
}
