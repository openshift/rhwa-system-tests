package tests

import (
	"context"
	"os"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/mustgather"
	"github.com/medik8s/system-tests/tests/mdr-operator/internal/mdrparams"
)

var _ = Describe("MDR Observability Tests",
	Serial,
	Ordered,
	ContinueOnFailure,
	Label(labels.OperatorMDR, mdrparams.Label,
		labels.TierAcceptance, labels.FrequencyWeekly),
	func() {
		var ctx context.Context

		BeforeAll(func() {
			ctx = context.Background()

			By("Verifying MDR operator deployment is ready")

			mdrDeployment, err := deployment.Pull(
				APIClient, mdrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get MDR deployment")
			Expect(mdrDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"MDR deployment is not Ready")

			By("Pre-cleaning stale must-gather test resources from previous runs")

			cleanupMDRCR(mdrparams.MDRMustGatherTestName)
			cleanupMDRT(mdrparams.MDRTMustGatherTestName)
		})

		AfterAll(func() {
			By("Verifying MDR controller pod is running")

			Eventually(verifyMDRControllerRunning,
				medik8sparams.DefaultTimeout, mdrparams.DefaultPollInterval).Should(Succeed(),
				"MDR controller pod should be running after the must-gather test")
		})

		Context("must-gather", func() {
			It("collects MDR data via oc adm must-gather",
				Label(labels.DisruptionNonDestructive, labels.PlatformAny,
					labels.ComponentController),
				reportxml.ID("62581"),
				func() {
					By("Verifying the must-gather MDR CR name does not match a cluster node")

					// MDR treats a remediation's name as the node it targets, so a name
					// collision here would delete a real Machine.
					nodeObj := &corev1.Node{}
					nodeErr := APIClient.Get(ctx,
						client.ObjectKey{Name: mdrparams.MDRMustGatherTestName}, nodeObj)
					Expect(k8serrors.IsNotFound(nodeErr)).To(BeTrue(),
						"Node %q must not exist in the cluster to avoid triggering real remediation",
						mdrparams.MDRMustGatherTestName)

					By("Creating temporary must-gather output directory")

					mustGatherDir, mkdirErr := os.MkdirTemp("", "mdr-must-gather-*")
					Expect(mkdirErr).ToNot(HaveOccurred(), "Failed to create must-gather output directory")

					DeferCleanup(func() {
						if removeErr := os.RemoveAll(mustGatherDir); removeErr != nil {
							GinkgoWriter.Printf("WARNING: failed to remove must-gather dir %s: %v\n",
								mustGatherDir, removeErr)
						}
					})

					By("Creating the MDR template CR for must-gather to collect")

					mdrtCR := buildMDRT(mdrparams.MDRTMustGatherTestName)
					Expect(APIClient.Create(ctx, mdrtCR)).To(Succeed(),
						"Failed to create MDRT %s", mdrparams.MDRTMustGatherTestName)

					DeferCleanup(func() { cleanupMDRT(mdrparams.MDRTMustGatherTestName) })

					By("Creating the MDR remediation CR for must-gather to collect")

					mdrCR := buildMDR(mdrparams.MDRMustGatherTestName)
					Expect(APIClient.Create(ctx, mdrCR)).To(Succeed(),
						"Failed to create MDR %s", mdrparams.MDRMustGatherTestName)

					DeferCleanup(func() { cleanupMDRCR(mdrparams.MDRMustGatherTestName) })

					By("Capturing cluster state to validate the collection against")

					nodeNames := listNodeNames(ctx)
					controllerPodName := resolveMDRControllerPodName()
					GinkgoWriter.Printf("Validating against %d node(s), controller pod %s\n",
						len(nodeNames), controllerPodName)

					By("Running oc adm must-gather")

					mustGatherImage := os.Getenv(mdrparams.MustGatherImageEnvVar)
					if mustGatherImage == "" {
						mustGatherImage = mdrparams.DefaultMustGatherImage
					}

					Expect(mustgather.Run(ctx, mustGatherImage, mustGatherDir, mustgather.Options{
						CommandTimeout:   mdrparams.MustGatherTimeout,
						ImageInfoTimeout: mdrparams.OcDebugTimeout,
					}, GinkgoWriter.Printf)).To(Succeed(), "oc adm must-gather failed")

					By("Validating must-gather output contains MDR data")

					expectations := buildMDRMustGatherExpectations(nodeNames, controllerPodName)
					missingItems := mustgather.ValidateMustGatherContents(mustGatherDir, expectations)
					Expect(missingItems).To(BeEmpty(),
						"Must-gather validation failed:\n%s", strings.Join(missingItems, "\n"))
				})
		})
	})

// listNodeNames returns the names of every node in the cluster, sorted for
// deterministic expectation ordering.
func listNodeNames(ctx context.Context) []string {
	GinkgoHelper()
	nodeList := &corev1.NodeList{}
	Expect(APIClient.List(ctx, nodeList)).To(Succeed(), "Failed to list cluster nodes")
	Expect(nodeList.Items).ToNot(BeEmpty(), "Cluster has no nodes")
	names := make([]string, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		names = append(names, nodeList.Items[i].Name)
	}

	sort.Strings(names)

	return names
}

// resolveMDRControllerPodName returns the name of the running MDR controller
// pod, which must-gather is expected to collect resources and logs for.
func resolveMDRControllerPodName() string {
	GinkgoHelper()
	mdrPods, listErr := pod.List(APIClient, medik8sparams.OperatorNs, metav1.ListOptions{
		LabelSelector: mdrparams.OperatorControllerPodLabelSelector,
	})
	Expect(listErr).ToNot(HaveOccurred(), "Failed to list MDR controller pods")
	controllerPods := helpers.FilterPodsByDeployment(mdrPods, mdrparams.OperatorDeploymentName)
	Expect(controllerPods).ToNot(BeEmpty(), "No MDR controller pods found in %s",
		medik8sparams.OperatorNs)
	names := make([]string, 0, len(controllerPods))
	for _, controllerPod := range controllerPods {
		names = append(names, controllerPod.Object.Name)
	}

	sort.Strings(names)

	return names[0]
}

// buildMDRMustGatherExpectations lists every artifact OCP-62581 requires the
// must-gather archive to contain.
func buildMDRMustGatherExpectations(
	nodeNames []string, controllerPodName string,
) []mustgather.MustGatherExpectation {
	expectations := make([]mustgather.MustGatherExpectation, 0,
		len(nodeNames)+len(mdrparams.MDRCRDNames)+5)

	for _, nodeName := range nodeNames {
		expectations = append(expectations, mustgather.MustGatherExpectation{
			Description:  "node YAML for " + nodeName,
			PathContains: "nodes",
			NameGlob:     nodeName + ".yaml",
			MinCount:     1,
		})
	}

	for _, crdName := range mdrparams.MDRCRDNames {
		expectations = append(expectations, mustgather.MustGatherExpectation{
			Description: "MDR CRD definition " + crdName,
			NameGlob:    crdName + ".yaml",
			MinCount:    1,
		})
	}

	return append(expectations,
		mustgather.MustGatherExpectation{
			Description: "MDR template CR " + mdrparams.MDRTMustGatherTestName,
			NameGlob:    mdrparams.MDRTMustGatherTestName + ".yaml",
			MinCount:    1,
		},
		mustgather.MustGatherExpectation{
			Description: "MDR remediation CR " + mdrparams.MDRMustGatherTestName,
			NameGlob:    mdrparams.MDRMustGatherTestName + ".yaml",
			MinCount:    1,
		},
		mustgather.MustGatherExpectation{
			Description:  "MDR controller pod resources",
			PathContains: controllerPodName,
			MinCount:     1,
		},
		mustgather.MustGatherExpectation{
			Description:  "MDR controller pod logs",
			PathContains: controllerPodName,
			NameGlob:     "*.log",
			MinCount:     1,
		},
		mustgather.MustGatherExpectation{
			Description:  "Machine API namespace resources",
			PathContains: mdrparams.MachineAPINamespace,
			NameGlob:     "*.yaml",
			MinCount:     1,
		},
	)
}
