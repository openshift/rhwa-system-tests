package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/internal/mustgather"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("NHC Must-Gather Collection",
	Serial, Ordered,
	Label(labels.OperatorNHC, nhcparams.Label,
		labels.TierAcceptance, labels.FrequencyWeekly),
	func() {
		var ctx context.Context

		BeforeAll(func() {
			ctx = context.Background()

			By("Checking SSH access is available for kubelet stop/start")

			if !isSSHAvailable() {
				skipWithReason("SSH not available -- NHC must-gather test requires SSH access to stop kubelet")
			}

			By("Checking SNR CRD is installed (used as remediator in this test)")

			if !isSNRCRDInstalled(ctx) {
				skipWithReason("SelfNodeRemediation CRD not found -- NHC must-gather test requires SNR as remediator")
			}

			By("Checking the SNR controller is running, not merely installed")

			// buildNHCForWorkers points at SNRTemplateName; if SNR is installed but
			// not running, NHC goes Disabled and the failure would surface as an
			// opaque "did not reach Enabled" timeout with no mention of SNR.
			snrDeployment, snrErr := deployment.Pull(
				APIClient, nhcparams.SNRDeploymentName, medik8sparams.OperatorNs)
			if snrErr != nil || !snrDeployment.IsReady(medik8sparams.DefaultTimeout) {
				skipWithReason(fmt.Sprintf(
					"SNR deployment %q is not Ready -- NHC must-gather test requires a working remediator",
					nhcparams.SNRDeploymentName))
			}

			By("Verifying NHC controller deployment is ready")

			verifyNHCDeploymentReady()

			By("Verifying sufficient worker nodes")

			workerCount, err := helpers.CountReadyWorkerNodes(ctx, APIClient)
			Expect(err).ToNot(HaveOccurred(), "Failed to count Ready worker nodes")

			if workerCount < nhcparams.MinWorkersForMustGather {
				skipWithReason(fmt.Sprintf(
					"NHC must-gather test requires at least %d Ready worker nodes, found %d",
					nhcparams.MinWorkersForMustGather, workerCount))
			}
		})

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() {
				GinkgoWriter.Println("Test failed - collecting NHC controller diagnostics")
				logNHCControllerState()
			}
		})

		Context("must-gather", func() {
			It("collects NHC data via oc adm must-gather during active remediation",
				Label(labels.DisruptionDestructive, labels.PlatformAny, labels.ComponentController),
				reportxml.ID("51086"),
				func() {
					nhcName := nhcparams.NHCMustGatherTestName

					By("Selecting a worker node hosting neither an SNR nor an NHC controller pod")

					// Breaking the SNR controller's host would take down the remediator.
					// Breaking an NHC controller's host evicts a pod whose collected data
					// this spec asserts on, so those nodes are excluded too.
					excludedNodes, err := controllerNodeNames(
						nhcparams.SNRControllerPodLabelSelector,
						nhcparams.OperatorControllerPodLabelSelector)
					Expect(err).ToNot(HaveOccurred(), "Failed to locate operator controller pods")
					Expect(excludedNodes).ToNot(BeEmpty(),
						"No SNR or NHC controller pods matched their selectors -- "+
							"cannot safely exclude the remediator's host")
					GinkgoWriter.Printf("Excluding controller nodes: %v\n", excludedNodes)
					targetNode, err := helpers.SelectWorkerNode(ctx, APIClient, excludedNodes...)
					Expect(err).ToNot(HaveOccurred(), "Failed to select a worker node to remediate")
					targetName := targetNode.Name
					GinkgoWriter.Printf("Target node for must-gather test: %s\n", targetName)

					By("Recording boot ID before remediation")

					oldBootID, err := helpers.GetNodeBootIDFromAPI(ctx, APIClient, targetName)
					Expect(err).ToNot(HaveOccurred(), "Must read boot ID from node %s", targetName)

					By("Pre-cleaning stale NHC and SNR CRs")

					// Any leftover worker-selecting NHC CR from a sibling spec would win
					// the "only one NHC remediates at a time" race and this spec's CR
					// would never reach Remediating.
					for _, staleName := range nhcparams.WorkerSelectingNHCNames {
						cleanupNHCCR(ctx, staleName)
						waitForNHCGone(ctx, staleName)
					}

					cleanupSNRCR(ctx, targetName)

					By("Creating artifact directory for must-gather output")

					mustGatherDir, mkdirErr := mustgather.CreateDestDir(
						"nhc-must-gather-", GinkgoT().TempDir())
					Expect(mkdirErr).ToNot(HaveOccurred(),
						"Failed to create must-gather output directory")
					GinkgoWriter.Printf("must-gather output directory: %s\n", mustGatherDir)

					// Registered before the kubelet is stopped so recovery still runs if
					// the spec fails between the stop and the gather.
					DeferCleanup(func() {
						By("Cleaning up NHC and SNR CRs for " + targetName)
						cleanupNHCCR(ctx, nhcName)
						cleanupSNRCR(ctx, targetName)

						By("Restarting kubelet on " + targetName)

						if sshErr := startKubeletForRemediationRetrying(ctx, targetName); sshErr != nil {
							GinkgoWriter.Printf("WARNING: SSH kubelet restart failed for %s: %v\n",
								targetName, sshErr)
							AddReportEntry("ssh-kubelet-restart-failed",
								fmt.Sprintf("node %s: %v", targetName, sshErr))
						}

						By("Waiting for node " + targetName + " to become Ready")

						if recoverErr := helpers.WaitForNodeReady(
							ctx, APIClient, targetName,
							nhcparams.DefaultPollInterval, nhcparams.NodeReadyTimeout,
							GinkgoWriter.Printf); recoverErr != nil {
							GinkgoWriter.Printf("WARNING: node %s did not recover: %v\n",
								targetName, recoverErr)
							AddReportEntry("safety-net-recovery-failed",
								fmt.Sprintf("node %s did not recover: %v", targetName, recoverErr))
						}
					})

					By("Creating NHC CR " + nhcName + " for workers")

					Expect(APIClient.Create(ctx, buildNHCForWorkers(nhcName))).To(Succeed(),
						"Failed to create NHC CR %q", nhcName)

					By("Waiting for NHC to reach Enabled before breaking a node")

					Expect(waitForNHCPhase(ctx, nhcName, nhcparams.NHCPhaseEnabled,
						medik8sparams.DefaultTimeout)).To(Succeed(),
						"NHC CR %q did not reach %s", nhcName, nhcparams.NHCPhaseEnabled)

					By("Stopping kubelet on " + targetName + " to trigger remediation")

					Expect(stopKubeletForRemediation(ctx, targetName)).To(Succeed(),
						"Failed to stop kubelet on %s", targetName)

					By("Waiting for NHC to enter Remediating -- the gather must run while remediation is active")

					Expect(waitForNHCPhase(ctx, nhcName, nhcparams.NHCPhaseRemediating,
						nhcparams.NodeNotReadyTimeout)).To(Succeed(),
						"NHC CR %q did not reach %s", nhcName, nhcparams.NHCPhaseRemediating)

					By("Confirming the SNR CR exists for " + targetName)

					Eventually(ctx, func() (bool, error) {
						return snrCRExists(ctx, targetName)
					}).WithPolling(nhcparams.DefaultPollInterval).
						WithTimeout(nhcparams.NodeNotReadyTimeout).Should(BeTrue(),
						"SNR CR was not created for node %s", targetName)

					By("Listing NHC controller pods whose collected data is asserted on")

					nhcPodNames, podErr := controllerPodNames(nhcparams.OperatorControllerPodLabelSelector)
					Expect(podErr).ToNot(HaveOccurred(), "Failed to list NHC controller pods")
					Expect(nhcPodNames).ToNot(BeEmpty(), "No NHC controller pods found")
					GinkgoWriter.Printf("NHC controller pods: %v\n", nhcPodNames)

					By("Resolving the must-gather image")

					mustGatherImage := mustgather.ResolveImage(
						mustgather.ImageEnvVar, mustgather.DefaultImage, GinkgoWriter.Printf)
					Expect(mustGatherImage).To(ContainSubstring(":"),
						"must-gather image %q should contain a tag separator", mustGatherImage)

					By("Running oc adm must-gather")

					testStartTime := time.Now()

					DeferCleanup(func() {
						By("Cleaning up leftover must-gather namespaces")
						mustgather.CleanupNamespaces(context.Background(), testStartTime,
							mustgather.DefaultCleanupTimeout, GinkgoWriter.Printf)
					})

					Expect(mustgather.Run(ctx, mustGatherImage, mustGatherDir, mustgather.Options{
						CommandTimeout:   nhcparams.MustGatherTimeout,
						ImageInfoTimeout: mustgather.DefaultImageInfoTimeout,
						OCTimeout:        nhcparams.MustGatherOCTimeout,
						SaveCommandLog:   true,
						HomeFallback:     true,
					}, GinkgoWriter.Printf)).To(Succeed(), "oc adm must-gather failed")

					By("Validating must-gather output contains NHC data")

					expectations := []mustgather.MustGatherExpectation{
						{
							Description:  "YAML for the node under remediation",
							PathContains: "nodes",
							NameGlob:     targetName + ".yaml",
							MinCount:     1,
						},
						{
							// Deployments are collected as one aggregate file per group,
							// not one file per deployment -- verified against the image on
							// a live cluster. Narrower than matching any *.yaml under the
							// namespace, which a stray ConfigMap would satisfy.
							Description:  "workloads in the NHC operator namespace",
							PathContains: medik8sparams.OperatorNs + "/apps",
							NameGlob:     "deployments.yaml",
							MinCount:     1,
						},
						{
							Description: "active NodeHealthCheck CR instance",
							NameGlob:    nhcName + ".yaml",
							MinCount:    1,
						},
						{
							// The only expectation that cannot be satisfied on an idle
							// cluster: this file exists only if the gather ran while the
							// SNR CR was live, so it makes the Remediating ordering above
							// self-verifying rather than merely conventional.
							Description:  "SelfNodeRemediation CR collected during active remediation",
							PathContains: "self-node-remediation.medik8s.io/selfnoderemediations",
							NameGlob:     "*.yaml",
							MinCount:     1,
						},
					}
					for _, crdName := range nhcparams.NHCCRDNames {
						expectations = append(expectations, mustgather.MustGatherExpectation{
							Description: "NHC CRD definition " + crdName,
							NameGlob:    crdName + ".yaml",
							MinCount:    1,
						})
					}

					for _, podName := range nhcPodNames {
						expectations = append(expectations, mustgather.MustGatherExpectation{
							Description:  "collected data for NHC controller pod " + podName,
							PathContains: podName,
							MinCount:     1,
						})
					}

					missingItems := mustgather.ValidateMustGatherContents(mustGatherDir, expectations)
					if len(missingItems) > 0 {
						logCollectedPaths(mustGatherDir)
					}

					Expect(missingItems).To(BeEmpty(),
						"Must-gather validation failed:\n%s", strings.Join(missingItems, "\n"))

					By("Waiting for SNR remediation to complete (node reboot)")

					Expect(waitForSNRRemediationComplete(ctx, targetName, oldBootID)).To(Succeed(),
						"SNR remediation should complete for node %s", targetName)

					By("Waiting for NHC to return to Enabled after remediation")

					Expect(waitForNHCPhase(ctx, nhcName, nhcparams.NHCPhaseEnabled,
						nhcparams.RemediationCompletionTimeout)).To(Succeed(),
						"NHC CR %q did not return to %s", nhcName, nhcparams.NHCPhaseEnabled)
				})
		})
	})

// skipWithReason skips the spec and records the reason as a report entry, so a
// skipped run is attributable from the JUnit artifact rather than only from the
// console log. A skipped Ginkgo spec reports Skipped, not Failed, so a green
// suite is otherwise no evidence this spec executed.
func skipWithReason(reason string) {
	GinkgoHelper()
	AddReportEntry("skipped-reason", reason)
	Skip(reason)
}

// logCollectedPaths dumps what must-gather actually collected, so a validation
// failure can be diagnosed from the CI log without the output directory.
func logCollectedPaths(destDir string) {
	GinkgoHelper()
	collected, walkErr := mustgather.CollectRelativePaths(destDir)
	if walkErr != nil {
		GinkgoWriter.Printf("WARNING: failed to list collected paths in %s: %v\n", destDir, walkErr)

		return
	}

	GinkgoWriter.Printf("must-gather collected %d paths:\n%s\n",
		len(collected), strings.Join(collected, "\n"))
}

// listControllerPods returns the non-terminating pods in the operator namespace
// matching a label selector. Pods carrying a DeletionTimestamp are skipped: on a
// node whose kubelet was stopped they are being evicted, so their names would be
// captured here and gone by the time must-gather runs.
func listControllerPods(labelSelector string) ([]*pod.Builder, error) {
	pods, err := pod.List(APIClient, medik8sparams.OperatorNs,
		metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods with selector %q: %w", labelSelector, err)
	}

	live := make([]*pod.Builder, 0, len(pods))

	for _, listedPod := range pods {
		if listedPod.Object.DeletionTimestamp != nil {
			continue
		}

		live = append(live, listedPod)
	}

	return live, nil
}

// controllerPodNames returns the names of the live pods matching a label selector.
func controllerPodNames(labelSelector string) ([]string, error) {
	pods, err := listControllerPods(labelSelector)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(pods))
	for _, listedPod := range pods {
		names = append(names, listedPod.Object.Name)
	}

	return names, nil
}

// controllerNodeNames returns the distinct node names hosting live pods that
// match any of the given label selectors.
func controllerNodeNames(labelSelectors ...string) ([]string, error) {
	seen := make(map[string]bool)
	nodeNames := make([]string, 0, len(labelSelectors))

	for _, labelSelector := range labelSelectors {
		pods, err := listControllerPods(labelSelector)
		if err != nil {
			return nil, err
		}

		for _, controllerPod := range pods {
			nodeName := controllerPod.Object.Spec.NodeName
			if nodeName == "" || seen[nodeName] {
				continue
			}

			seen[nodeName] = true
			nodeNames = append(nodeNames, nodeName)
		}
	}

	return nodeNames, nil
}
