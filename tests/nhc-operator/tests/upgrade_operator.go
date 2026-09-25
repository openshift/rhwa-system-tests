//nolint:lll // Scenario descriptions and exact evidence fields are more useful when kept intact.
package tests

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("NHC Upgrade Operator", Serial, Ordered,
	Label(labels.OperatorNHC, nhcparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionDestructive, labels.PlatformAny, labels.ComponentOLM,
		labels.ComponentRemediation), func() {
		var (
			ctx               context.Context
			inputs            nhcparams.UpgradeOperatorInputs
			oldCSV            *olm.ClusterServiceVersionBuilder
			configUID         string
			configSpec        map[string]interface{}
			owned             *nhcutils.OwnedRun
			currentTargetNode string
		)

		BeforeAll(func() {
			ctx = context.Background()

			var err error

			inputs, err = nhcparams.LoadUpgradeOperatorInputs()
			Expect(err).NotTo(HaveOccurred())
			inputs, err = nhcutils.ResolveAndVerifyUpgradeOperatorInputs(ctx, inputs)
			Expect(err).NotTo(HaveOccurred())
			Expect(inputs.Namespace).To(Equal(medik8sparams.OperatorNs), "NHC uses its established operator namespace")
			sdkVersion, err := nhcutils.RunOperatorSDK(ctx, inputs.OperatorSDK, "version")
			Expect(err).NotTo(HaveOccurred())
			Expect(sdkVersion).To(ContainSubstring("v1.42.2"))
			AddReportEntry("nhc-upgrade-sdk", sdkVersion)

			clusterVersion := &configv1.ClusterVersion{}
			Expect(APIClient.Get(ctx, client.ObjectKey{Name: "version"}, clusterVersion)).To(Succeed())
			Expect(clusterVersion.Status.Desired.Version).To(HavePrefix("5.0."), "requires an OpenShift 5.0 cluster")
			AddReportEntry("nhc-upgrade-operator-inputs", inputs)
		})

		BeforeEach(func() {
			By("rejecting leftover resources owned by this standalone scenario")
			Expect(nhcutils.CheckClean(ctx, APIClient, inputs.Namespace)).To(Succeed())
			owned = &nhcutils.OwnedRun{API: APIClient, Namespace: inputs.Namespace,
				Token: rand.Text(), SDK: inputs.OperatorSDK, CleanupPackage: nhcutils.CleanupBundle}
			// Register before CREATE/install, including partial failures. A rejected
			// preflight never registers or invokes package cleanup.
			if inputs.SkipCleanup {
				GinkgoWriter.Println("WARNING: NHC_UPGRADE_SKIP_CLEANUP=true; preserving test resources for debugging")
				AddReportEntry("nhc-upgrade-cleanup", "skipped by NHC_UPGRADE_SKIP_CLEANUP=true")
			} else {
				DeferCleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
					defer cancel()

					err := owned.Cleanup(cleanupCtx)
					if err != nil {
						AddReportEntry("nhc-upgrade-cleanup-failure", err.Error())
						AddReportEntry("nhc-upgrade-cleanup-evidence", nhcutils.CollectFailureEvidence(cleanupCtx, inputs.Namespace))
					}
				})
			}
		})

		JustAfterEach(func() {
			cleanupNHCCR(ctx, nhcparams.ClusterUpgradeTestName)

			if currentTargetNode != "" {
				nodeName := currentTargetNode
				currentTargetNode = ""

				cleanupSNRCR(ctx, nodeName)

				if isSSHAvailable() {
					if err := startKubeletForRemediation(ctx, nodeName); err != nil {
						GinkgoWriter.Printf("WARNING: SSH kubelet restart failed for %s: %v\n", nodeName, err)
						AddReportEntry("ssh-kubelet-restart-failed", fmt.Sprintf("node %s: %v", nodeName, err))
					}
				}

				if err := helpers.WaitForNodeReady(ctx, APIClient, nodeName,
					nhcparams.DefaultPollInterval, nhcparams.NodeReadyTimeout, GinkgoWriter.Printf); err != nil {
					GinkgoWriter.Printf("WARNING: node %s did not recover: %v\n", nodeName, err)
					AddReportEntry("upgrade-recovery-failed", fmt.Sprintf("node %s: %v", nodeName, err))
				}

				if medik8sparams.KubeletStopViaOCDebug {
					if err := helpers.RemoveKubeletStopGuard(
						ctx, nodeName, nhcparams.OCDebugKubeletStopTimeout); err != nil {
						GinkgoWriter.Printf("WARNING: failed to remove kubelet-stop guard on %s: %v\n", nodeName, err)
					}
				}
			}

			if CurrentSpecReport().Failed() && ctx != nil && inputs.Namespace != "" {
				AddReportEntry("nhc-upgrade-failure-evidence", nhcutils.CollectFailureEvidence(ctx, inputs.Namespace))
			}
		})

		It("upgrades its preserved configuration and completes candidate remediation", reportxml.ID("REPLACE_WITH_POLARION_ID"), func() {
			Expect(owned.CreateNamespace(ctx)).To(Succeed())
			By("installing the pinned SNR prerequisite and its remediation template")

			owned.Packages = append(owned.Packages, inputs.SNRPackage)
			output, err := nhcutils.InstallBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.BaselineSNR.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle (SNR) output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForUpgradeAPI(ctx, upgradeTemplate(inputs.Namespace))).To(Succeed())
			Expect(owned.Create(ctx, buildSNRT(nhcparams.NHCUpgradeTemplateName))).To(Succeed())
			Expect(waitForSNRTemplate(ctx, nhcparams.NHCUpgradeTemplateName)).To(Succeed())
			Expect(waitForSNRNodeAgents(ctx)).To(Succeed(), "SNR node agents were not ready")
			By("installing the resolved downstream NHC baseline bundle")

			owned.Packages = append(owned.Packages, inputs.Package)
			output, err = nhcutils.InstallBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.BaselineNHC.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle (baseline NHC) output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred())

			oldCSV = waitForNHCUpgradeCSV(inputs.Namespace, inputs.BaselineNHC.Version, inputs.BaselineNHC.Image, "baseline")
			oldImage, err := nhcutils.GetNHCControllerImage(APIClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldImage).To(Equal(inputs.BaselineNHC.Image))
			By("creating a safe, observable NodeHealthCheck configuration")
			Expect(waitForUpgradeAPI(ctx, upgradeNHC())).To(Succeed())
			nhc := upgradeNHC()
			nhc.Object["spec"] = nhcutils.SafeSpec(nhcparams.NHCUpgradeTemplateName, inputs.Namespace, owned.Token)
			Expect(owned.Create(ctx, nhc)).To(Succeed())
			Expect(waitForPauseResponse(ctx, nhc.GetUID(), owned.Token, nhc.GetResourceVersion())).To(Succeed())
			configUID, configSpec = captureNHCConfiguration(ctx)
			AddReportEntry("nhc-config-before-operator-upgrade", map[string]interface{}{
				"uid": configUID, "spec": configSpec,
			})
			GinkgoWriter.Printf("NHC config before operator upgrade: uid=%s spec=%v\n", configUID, configSpec)

			By("upgrading in place to the explicitly supplied candidate bundle")

			output, err = nhcutils.UpgradeBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.CandidateNHC.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle-upgrade output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred(), "the old operator must not be uninstalled before upgrade")
			By("requiring a new CSV and the candidate version and image")

			newCSV := waitForNHCUpgradeCSV(inputs.Namespace, inputs.CandidateNHC.Version, inputs.CandidateNHC.Image, "candidate")
			Expect(newCSV.Object.Name).NotTo(Equal(oldCSV.Object.Name), "version parity is not an upgrade")

			candidateImage, err := nhcutils.GetNHCControllerImage(APIClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidateImage).To(Equal(inputs.CandidateNHC.Image))
			By("verifying the same configuration identity, specification, and reconciliation")

			uid, spec := captureNHCConfiguration(ctx)
			Expect(uid).To(Equal(configUID), "upgrade must preserve the existing NodeHealthCheck")
			Expect(spec).To(Equal(configSpec), "upgrade must preserve the NodeHealthCheck specification")
			By("requiring a fresh candidate-controller response to a unique pause request")

			probe := owned.Token + "-candidate"
			changeUpgradePause(ctx, types.UID(configUID), probe)
			By("restoring the original configuration and requiring another controller response")
			changeUpgradePause(ctx, types.UID(configUID), owned.Token)
			uid, spec = captureNHCConfiguration(ctx)
			Expect(uid).To(Equal(configUID))
			Expect(spec).To(Equal(configSpec))
			AddReportEntry("nhc-config-after-operator-upgrade", map[string]interface{}{
				"uid": uid, "spec": spec,
			})
			GinkgoWriter.Printf("NHC config after operator upgrade: uid=%s spec=%v\n", uid, spec)

			By("requiring a real remediation from the upgraded candidate")

			currentTargetNode, err = upgradeRunRemediationCycle(
				ctx, "post-operator-upgrade", nhcparams.NHCUpgradeTemplateName)
			Expect(err).NotTo(HaveOccurred())
			AddReportEntry("nhc-upgrade-remediation-node", currentTargetNode)
			cleanupPostRemediationNHC(ctx, &currentTargetNode, "post-operator-upgrade")
		})
	})
