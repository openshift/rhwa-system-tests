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
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("SBR Fresh Install", Serial, Ordered,
	Label(labels.OperatorSBR, sbrparams.Label, labels.TierFreshInstall,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM), func() {
		var (
			ctx    context.Context
			inputs sbrparams.FreshInstallInputs
			owned  *sbrutils.OwnedRun
		)

		BeforeAll(func() {
			ctx = context.Background()

			var err error

			inputs, err = sbrparams.LoadFreshInstallInputs()
			Expect(err).NotTo(HaveOccurred())
			inputs, err = sbrutils.ResolveAndVerifyFreshInstallInputs(ctx, inputs)
			Expect(err).NotTo(HaveOccurred())
			Expect(inputs.Namespace).To(Equal(medik8sparams.OperatorNs), "SBR uses its established operator namespace")
			sdkVersion, err := sbrutils.RunOperatorSDK(ctx, inputs.OperatorSDK, "version")
			Expect(err).NotTo(HaveOccurred())
			Expect(sdkVersion).To(ContainSubstring("v1.42.2"))
			AddReportEntry("sbr-fresh-install-sdk", sdkVersion)

			clusterVersion := &configv1.ClusterVersion{}
			Expect(APIClient.Get(ctx, client.ObjectKey{Name: "version"}, clusterVersion)).To(Succeed())
			Expect(clusterVersion.Status.Desired.Version).To(HavePrefix("5.0."), "requires an OpenShift 5.0 cluster")
			AddReportEntry("sbr-fresh-install-inputs", inputs)
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
				AddReportEntry("sbr-fresh-install-cleanup", "skipped by SBR_UPGRADE_SKIP_CLEANUP=true")
			} else {
				DeferCleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
					defer cancel()

					err := owned.Cleanup(cleanupCtx)
					if err != nil {
						AddReportEntry("sbr-fresh-install-cleanup-failure", err.Error())
						AddReportEntry("sbr-fresh-install-cleanup-evidence", sbrutils.CollectFailureEvidence(cleanupCtx, inputs.Namespace))
					}
				})
			}
		})

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() && ctx != nil && inputs.Namespace != "" {
				AddReportEntry("sbr-fresh-install-failure-evidence", sbrutils.CollectFailureEvidence(ctx, inputs.Namespace))
			}
		})

		It("installs the pinned candidate on a clean cluster and reconciles a probe configuration",
			reportxml.ID("REPLACE_WITH_POLARION_ID"), func() {
				Expect(owned.CreateNamespace(ctx)).To(Succeed())
				By("fresh-installing the explicitly pinned candidate bundle")

				owned.Packages = append(owned.Packages, inputs.Package)
				output, err := sbrutils.InstallBundle(ctx, inputs.OperatorSDK, inputs.Namespace, inputs.CandidateSBR.Bundle)
				GinkgoWriter.Printf("operator-sdk run bundle (candidate SBR) output:\n%s\n", output)
				Expect(err).NotTo(HaveOccurred())

				waitForSBRUpgradeCSV(inputs.Namespace, inputs.CandidateSBR.Version, inputs.CandidateSBR.Image, "fresh candidate")

				candidateImage, err := sbrutils.GetSBRControllerImage(APIClient)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidateImage).To(Equal(inputs.CandidateSBR.Image))

				By("requiring a fresh response from the newly installed controller")
				Expect(waitForUpgradeAPI(ctx, upgradeSBRC())).To(Succeed())

				storageClass := discoverRWXStorageClass()
				sbrc := buildSBRC(sbrparams.SBRUpgradeConfigTestName, sbrutils.SafeSpec(owned.Token, storageClass))
				Expect(owned.Create(ctx, sbrc)).To(Succeed())
				waitForUpgradeAgentDaemonSetExists(ctx)

				uid, spec := captureSBRCConfiguration(ctx)
				AddReportEntry("sbr-config-fresh-install", map[string]interface{}{
					"uid": uid, "spec": spec,
				})
				GinkgoWriter.Printf("SBR config after fresh install: uid=%s spec=%v\n", uid, spec)

				By("Step 3: Validate freshly installed SBR via real remediation cycle")

				err = upgradeRunSBRRemediationCycle(ctx, owned)
				Expect(err).NotTo(HaveOccurred(), "SBR remediation failed after fresh install")
			})
	})
