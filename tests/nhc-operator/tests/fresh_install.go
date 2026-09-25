//nolint:lll // Exact evidence fields and scenario descriptions are more useful when kept intact.
package tests

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("NHC Fresh Install", Serial,
	Label(labels.OperatorNHC, nhcparams.Label, labels.TierFreshInstall,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM), func() {
		var (
			ctx    context.Context
			inputs nhcparams.FreshInstallInputs
			owned  *nhcutils.OwnedRun
		)

		BeforeEach(func() {
			ctx = context.Background()

			var err error

			inputs, err = nhcparams.LoadFreshInstallInputs()
			Expect(err).NotTo(HaveOccurred())

			inputs, err = nhcutils.ResolveAndVerifyFreshInstallInputs(ctx, inputs)
			Expect(err).NotTo(HaveOccurred())
			Expect(inputs.Namespace).To(Equal(medik8sparams.OperatorNs))

			AddReportEntry("nhc-fresh-install-inputs", inputs)

			clusterVersion := &configv1.ClusterVersion{}
			Expect(APIClient.Get(ctx, client.ObjectKey{Name: "version"}, clusterVersion)).To(Succeed())
			Expect(clusterVersion.Status.Desired.Version).To(HavePrefix("5.0."), "requires an OpenShift 5.0 cluster")

			Expect(nhcutils.CheckClean(ctx, APIClient, inputs.Namespace)).To(Succeed())

			owned = &nhcutils.OwnedRun{
				API:            APIClient,
				Namespace:      inputs.Namespace,
				Token:          rand.Text(),
				SDK:            inputs.OperatorSDK,
				CleanupPackage: nhcutils.CleanupBundle,
			}

			if inputs.SkipCleanup {
				GinkgoWriter.Println("WARNING: NHC_UPGRADE_SKIP_CLEANUP=true; preserving test resources for debugging")
				AddReportEntry("nhc-fresh-install-cleanup", "skipped by NHC_UPGRADE_SKIP_CLEANUP=true")
			} else {
				DeferCleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
					defer cancel()

					err := owned.Cleanup(cleanupCtx)
					if err != nil {
						GinkgoWriter.Printf("WARNING: fresh-install cleanup failed: %v\n", err)
						AddReportEntry("nhc-fresh-install-cleanup-failure", err.Error())
						AddReportEntry("nhc-fresh-install-cleanup-evidence",
							nhcutils.CollectFailureEvidence(cleanupCtx, inputs.Namespace))
					}
				})
			}
		})

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() && inputs.Namespace != "" {
				AddReportEntry("nhc-fresh-install-failure-evidence", nhcutils.CollectFailureEvidence(ctx, inputs.Namespace))
			}
		})

		It("installs and reconciles the pinned candidate on a clean cluster", reportxml.ID("REPLACE_WITH_POLARION_ID"), func() {
			Expect(owned.CreateNamespace(ctx)).To(Succeed())

			By("installing the pinned SNR prerequisite")

			owned.Packages = append(owned.Packages, inputs.SNRPackage)
			output, err := nhcutils.InstallBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.CandidateSNR.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle (SNR) output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForUpgradeAPI(ctx, upgradeTemplate(inputs.Namespace))).To(Succeed())
			Expect(owned.Create(ctx, buildSNRT(nhcparams.NHCUpgradeTemplateName))).To(Succeed())
			Expect(waitForSNRTemplate(ctx, nhcparams.NHCUpgradeTemplateName)).To(Succeed())

			By("fresh-installing the explicitly pinned candidate bundle")

			owned.Packages = append(owned.Packages, inputs.Package)
			output, err = nhcutils.InstallBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.CandidateNHC.Bundle)
			GinkgoWriter.Printf("operator-sdk run bundle (candidate NHC) output:\n%s\n", output)
			Expect(err).NotTo(HaveOccurred())
			waitForNHCUpgradeCSV(inputs.Namespace, inputs.CandidateNHC.Version, inputs.CandidateNHC.Image, "fresh candidate")

			candidateImage, err := nhcutils.GetNHCControllerImage(APIClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidateImage).To(Equal(inputs.CandidateNHC.Image))

			By("requiring a fresh response from the newly installed controller")
			Expect(waitForUpgradeAPI(ctx, upgradeNHC())).To(Succeed())
			nhc := upgradeNHC()
			nhc.Object["spec"] = nhcutils.SafeSpec(nhcparams.NHCUpgradeTemplateName, inputs.Namespace, owned.Token)
			Expect(owned.Create(ctx, nhc)).To(Succeed())
			Expect(waitForPauseResponse(ctx, nhc.GetUID(), owned.Token, nhc.GetResourceVersion())).To(Succeed())
		})
	})
