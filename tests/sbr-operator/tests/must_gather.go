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
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe(
	"SBR Must-Gather Diagnostics",
	Serial,
	Ordered,
	Label(labels.OperatorSBR), func() {
		It("Verify SBR must-gather collects diagnostic data",
			reportxml.ID("88733"),
			Label(
				labels.DisruptionNonDestructive,
				labels.TierAcceptance,
				labels.PlatformAny,
				labels.ComponentController,
				labels.FrequencyWeekly,
			), func() {
				By("Verifying SBR deployment is Ready")

				sbrDeployment, err := deployment.Pull(
					APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
				Expect(err).ToNot(HaveOccurred(), "Failed to get SBR deployment")
				Expect(sbrDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
					"SBR deployment is not Ready")

				By("Detecting cluster topology")

				infraConfig, infraErr := infrastructure.Pull(APIClient)
				Expect(infraErr).ToNot(HaveOccurred(), "Failed to pull infrastructure configuration")

				if infraConfig.Object.Status.ControlPlaneTopology == configv1.ExternalTopologyMode {
					Skip("Must-gather test not supported on HyperShift clusters. " +
						"Node collection via 'oc adm inspect nodes' fails due to HyperShift API limitations (0 nodes collected), " +
						"and Machine API resources (MachineHealthCheck) exist only on the management cluster. " +
						"See: https://github.com/openshift/release/pull/83913")
				}

				By("Resolving the RHWA must-gather image")

				mustGatherImage := resolveMustGatherImage()
				Expect(mustGatherImage).To(ContainSubstring(":"),
					"must-gather image %q should contain a tag separator", mustGatherImage)
				GinkgoWriter.Printf("Using must-gather image: %s\n", mustGatherImage)

				By("Creating artifact directory for must-gather output")

				destDir := createMustGatherDestDir()

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

				By("Running oc adm must-gather")

				testStartTime := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), sbrparams.MustGatherContextTimeout)
				defer cancel()

				DeferCleanup(func() {
					By("Cleaning up leftover must-gather namespaces")
					mustgather.CleanupNamespaces(context.Background(), testStartTime,
						sbrparams.MustGatherCleanupTimeout, GinkgoWriter.Printf)
				})

				Expect(mustgather.Run(ctx, mustGatherImage, destDir, mustgather.Options{
					ImageInfoTimeout: sbrparams.MustGatherImageInfoTimeout,
					OCTimeout:        sbrparams.MustGatherOCTimeout,
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

				By("Validating SBR CRD definitions are present")

				for _, crdName := range sbrparams.SBRCRDNames {
					Expect(mustgather.HasMatchingFile(collectedFiles, crdName+".yaml")).To(BeTrue(),
						"must-gather should contain CRD definition for %s", crdName)
				}

				By("Validating MachineHealthCheck data is collected")

				Expect(mustgather.HasMatchingFile(collectedFiles, "machinehealthchecks")).To(BeTrue(),
					"must-gather should contain MachineHealthCheck data")
			})
	})

func resolveMustGatherImage() string {
	return mustgather.DiscoverImage(
		APIClient, medik8sparams.OperatorNs,
		sbrparams.MustGatherImageEnvVar, sbrparams.DefaultMustGatherImage, GinkgoWriter.Printf)
}

func createMustGatherDestDir() string {
	base := os.Getenv("ARTIFACT_DIR")
	if base == "" {
		base = GinkgoT().TempDir()
	}

	dir, mkdirErr := os.MkdirTemp(base, "sbr-must-gather-")
	ExpectWithOffset(1, mkdirErr).ToNot(HaveOccurred(), "Failed to create must-gather output directory")

	return dir
}
