//nolint:lll // Scenario descriptions and exact evidence fields are more useful when kept intact.
package tests

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("SBR Upgrade Operator", Serial, Ordered,
	Label(labels.OperatorSBR, sbrparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM), func() {
		var (
			ctx        context.Context
			inputs     sbrparams.UpgradeOperatorInputs
			oldCSV     *olm.ClusterServiceVersionBuilder
			configUID  string
			configSpec map[string]interface{}
			owned      *sbrutils.OwnedRun
		)

		BeforeAll(func() {
			ctx = context.Background()

			var err error

			inputs, err = sbrparams.LoadUpgradeOperatorInputs()
			Expect(err).NotTo(HaveOccurred())
			inputs, err = sbrutils.ResolveAndVerifyUpgradeOperatorInputs(ctx, inputs)
			Expect(err).NotTo(HaveOccurred())
			Expect(inputs.Namespace).To(Equal(medik8sparams.OperatorNs), "SBR uses its established operator namespace")
			sdkVersion, err := sbrutils.RunOperatorSDK(ctx, inputs.OperatorSDK, "version")
			Expect(err).NotTo(HaveOccurred())
			Expect(sdkVersion).To(ContainSubstring("v1.42.2"))
			AddReportEntry("sbr-upgrade-sdk", sdkVersion)

			clusterVersion := &configv1.ClusterVersion{}
			Expect(APIClient.Get(ctx, client.ObjectKey{Name: "version"}, clusterVersion)).To(Succeed())
			Expect(clusterVersion.Status.Desired.Version).To(HavePrefix("5.0."), "requires an OpenShift 5.0 cluster")
			AddReportEntry("sbr-upgrade-operator-inputs", inputs)
		})

		BeforeEach(func() {
			By("rejecting leftover resources owned by this standalone scenario")
			Expect(sbrutils.CheckClean(ctx, APIClient, inputs.Namespace)).To(Succeed())
			owned = &sbrutils.OwnedRun{API: APIClient, Namespace: inputs.Namespace,
				Token: rand.Text(), SDK: inputs.OperatorSDK, CleanupPackage: sbrutils.CleanupBundle}
			// Register before CREATE/install, including partial failures. A rejected
			// preflight never registers or invokes package cleanup.
			if inputs.SkipCleanup {
				GinkgoWriter.Println("WARNING: SBR_UPGRADE_SKIP_CLEANUP=true; preserving test resources for debugging")
				AddReportEntry("sbr-upgrade-cleanup", "skipped by SBR_UPGRADE_SKIP_CLEANUP=true")
			} else {
				DeferCleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
					defer cancel()

					err := owned.Cleanup(cleanupCtx)
					if err != nil {
						AddReportEntry("sbr-upgrade-cleanup-failure", err.Error())
						AddReportEntry("sbr-upgrade-cleanup-evidence", sbrutils.CollectFailureEvidence(cleanupCtx, inputs.Namespace))
					}
				})
			}
		})

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() && ctx != nil && inputs.Namespace != "" {
				AddReportEntry("sbr-upgrade-failure-evidence", sbrutils.CollectFailureEvidence(ctx, inputs.Namespace))
			}
		})

		It("installs a baseline bundle and upgrades its preserved configuration", reportxml.ID("REPLACE_WITH_POLARION_ID"), func() {
			Expect(owned.CreateNamespace(ctx)).To(Succeed())
			By("installing the resolved downstream SBR baseline bundle")

			owned.Packages = append(owned.Packages, inputs.Package)
			output, err := sbrutils.InstallBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.BaselineSBR.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle (baseline SBR) output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred())

			oldCSV = waitForSBRUpgradeCSV(inputs.Namespace, inputs.BaselineSBR.Version, inputs.BaselineSBR.Image, "baseline")
			oldImage, err := sbrutils.GetSBRControllerImage(APIClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldImage).To(Equal(inputs.BaselineSBR.Image))
			By("creating a safe, observable StorageBasedRemediationConfig")
			Expect(waitForUpgradeAPI(ctx, upgradeSBRC())).To(Succeed())

			storageClass := discoverRWXStorageClass()
			sbrc := buildSBRC(sbrparams.SBRUpgradeConfigTestName, sbrutils.SafeSpec(owned.Token, storageClass))
			Expect(owned.Create(ctx, sbrc)).To(Succeed())
			baselineGeneration := waitForUpgradeAgentDaemonSetExists(ctx)
			configUID, configSpec = captureSBRCConfiguration(ctx)
			AddReportEntry("sbr-config-before-operator-upgrade", map[string]interface{}{
				"uid": configUID, "spec": configSpec,
			})
			GinkgoWriter.Printf("SBR config before operator upgrade: uid=%s spec=%v\n", configUID, configSpec)

			By("upgrading in place to the explicitly supplied candidate bundle")

			output, err = sbrutils.UpgradeBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.CandidateSBR.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle-upgrade output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred(), "the old operator must not be uninstalled before upgrade")
			By("requiring a new CSV and the candidate version and image")

			newCSV := waitForSBRUpgradeCSV(inputs.Namespace, inputs.CandidateSBR.Version, inputs.CandidateSBR.Image, "candidate")
			Expect(newCSV.Object.Name).NotTo(Equal(oldCSV.Object.Name), "version parity is not an upgrade")

			candidateImage, err := sbrutils.GetSBRControllerImage(APIClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidateImage).To(Equal(inputs.CandidateSBR.Image))
			By("verifying the same configuration identity, specification, and reconciliation")

			uid, spec := captureSBRCConfiguration(ctx)
			Expect(uid).To(Equal(configUID), "upgrade must preserve the existing StorageBasedRemediationConfig")
			Expect(spec).To(Equal(configSpec), "upgrade must preserve the StorageBasedRemediationConfig specification")
			By("requiring a fresh candidate-controller response to a probe patch")

			probeGeneration := patchSBRCMaxConsecutiveFailures(
				ctx, types.UID(configUID), baselineGeneration, int64(sbrparams.SBRCMaxConsecutiveFailuresMin+1))

			By("restoring the original configuration and requiring another controller response")
			patchSBRCMaxConsecutiveFailures(
				ctx, types.UID(configUID), probeGeneration, int64(sbrparams.SBRCMaxConsecutiveFailuresMin))
			uid, spec = captureSBRCConfiguration(ctx)
			Expect(uid).To(Equal(configUID))
			Expect(spec).To(Equal(configSpec))
			AddReportEntry("sbr-config-after-operator-upgrade", map[string]interface{}{
				"uid": uid, "spec": spec,
			})
			GinkgoWriter.Printf("SBR config after operator upgrade: uid=%s spec=%v\n", uid, spec)

			By("Step 4: Validate upgraded SBR via real remediation cycle")

			err = upgradeRunSBRRemediationCycle(ctx, owned)
			Expect(err).NotTo(HaveOccurred(), "SBR remediation failed after upgrade")
		})
	})
