//nolint:wsl_v5 // Ginkgo spec registration flows logically across test phases.
package fbcsuite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"
)

// UpgradeOperatorFBCTest owns all operator-specific behavior and mutable state for one run.
type UpgradeOperatorFBCTest interface {
	Setup(ctx context.Context) error
	BeforeUpgrade(ctx context.Context) error
	AfterUpgrade(ctx context.Context) error
	Cleanup(ctx context.Context)
	FailureEvidence(ctx context.Context) interface{}
}

// NewUpgradeOperatorFBCTest creates fresh operator-specific state from validated inputs.
type NewUpgradeOperatorFBCTest func(inputs medik8sparams.FBCUpgradeInputs) UpgradeOperatorFBCTest

// UpgradeOperatorFBCConfig defines the parameters for an operator's FBC upgrade suite.
type UpgradeOperatorFBCConfig struct {
	OperatorName              string
	PackageName               string
	SubscriptionName          string
	CSVNamePattern            string
	Channel                   string
	DeploymentName            string
	ContainerName             string
	Labels                    []string
	PolarionID                string
	NewUpgradeOperatorFBCTest NewUpgradeOperatorFBCTest
}

// DefineFBCUpgradeSuite defines the standard Ginkgo test suite for an operator's FBC upgrade.
//
//nolint:funlen // Spec registration flows sequentially across all upgrade phases.
func DefineFBCUpgradeSuite(cfg UpgradeOperatorFBCConfig) bool {
	if cfg.NewUpgradeOperatorFBCTest == nil {
		panic("DefineFBCUpgradeSuite requires a NewUpgradeOperatorFBCTest")
	}

	reportPrefix := strings.ToLower(cfg.OperatorName)

	defineFBCUpgradeSuite := func() {
		var (
			ctx    context.Context
			inputs medik8sparams.FBCUpgradeInputs
			test   UpgradeOperatorFBCTest
		)

		BeforeAll(func() {
			ctx = context.Background()

			var err error
			inputs, err = medik8sparams.LoadFBCUpgradeInputs(cfg.OperatorName)
			Expect(err).NotTo(HaveOccurred())
			AddReportEntry(
				fmt.Sprintf("%s-upgrade-operator-fbc-inputs", reportPrefix),
				inputs,
			)
		})

		BeforeEach(func() {
			test = cfg.NewUpgradeOperatorFBCTest(inputs)
			Expect(test).NotTo(BeNil())
			Expect(test.Setup(ctx)).To(Succeed())

			if inputs.SkipCleanup {
				GinkgoWriter.Printf("WARNING: %s_FBC_SKIP_CLEANUP=true; preserving test resources\n", cfg.OperatorName)
				AddReportEntry(
					fmt.Sprintf("%s-upgrade-operator-fbc-cleanup", reportPrefix),
					"skipped",
				)
			} else {
				DeferCleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
					defer cancel()

					test.Cleanup(cleanupCtx)

					helpers.DeleteSubscription(APIClient, cfg.SubscriptionName, medik8sparams.OperatorNs, GinkgoWriter.Printf)
					helpers.DeleteStaleCSVsAndInstallPlans(APIClient, cfg.PackageName, medik8sparams.OperatorNs, GinkgoWriter.Printf)
					if err := helpers.DeleteCandidateCatalog(APIClient, inputs.CatalogName); err != nil {
						GinkgoWriter.Printf("WARNING: candidate CatalogSource cleanup failed: %v\n", err)
					}
				})
			}
		})

		JustAfterEach(func() {
			if !CurrentSpecReport().Failed() || test == nil || ctx == nil {
				return
			}

			evidence := test.FailureEvidence(ctx)
			if evidence == nil {
				return
			}

			AddReportEntry(
				fmt.Sprintf("%s-upgrade-operator-fbc-failure-evidence", reportPrefix),
				evidence,
			)
		})

		It("upgrades a GA subscription through the candidate file-based catalog", reportxml.ID(cfg.PolarionID), func() {
			channel := cfg.Channel
			if channel == "" {
				channel = inputs.Channel
			}

			spec := helpers.OperatorOLMSpec{
				Package:          cfg.PackageName,
				SubscriptionName: cfg.SubscriptionName,
				Namespace:        medik8sparams.OperatorNs,
				CSVNamePattern:   cfg.CSVNamePattern,
				Channel:          channel,
			}

			By("1. Ensuring operator namespace exists")
			Expect(helpers.EnsureNamespace(ctx, APIClient, spec.Namespace)).To(Succeed())

			By("2. Applying IDMS and waiting for MachineConfigPool rollout if needed")
			preIDMSGens, err := helpers.GetMCPGenerations(ctx)
			Expect(err).NotTo(HaveOccurred(), "capture MCP generations before IDMS apply")

			idmsChanged, err := helpers.ApplyIDMSFile(ctx, inputs.IDMSPath, GinkgoWriter.Printf)
			Expect(err).NotTo(HaveOccurred())

			if !idmsChanged {
				GinkgoWriter.Println("IDMS unchanged, skipping MachineConfigPool rollout wait")
			} else {
				By("Waiting for MachineConfigPool rollout")
				Expect(helpers.WaitForMCPRollout(
					ctx,
					preIDMSGens,
					medik8sparams.MCPDetectionTimeout,
					medik8sparams.MCPRolloutTimeout,
					10*time.Second,
					GinkgoWriter.Printf,
				)).To(Succeed())
			}

			By("3. Creating candidate CatalogSource and waiting for READY state")
			_, err = helpers.CreateCandidateCatalog(APIClient, inputs.CatalogName, inputs.CatalogImage)
			Expect(err).NotTo(HaveOccurred())
			Expect(helpers.WaitForCatalogReady(
				ctx, APIClient, inputs.CatalogName, medik8sparams.OperatorUpgradeTimeout, 10*time.Second,
			)).To(Succeed())

			By("4. Installing GA operator baseline from redhat-operators")
			_, err = helpers.InstallGAOperatorSubscription(
				APIClient, spec.SubscriptionName, spec.Namespace, medik8sparams.GAOperatorCatalog,
				medik8sparams.GACatalogNamespace, spec.Package, spec.Channel,
			)
			Expect(err).NotTo(HaveOccurred())

			baseline, err := helpers.WaitForInstalledOperator(
				ctx, APIClient, spec, medik8sparams.OperatorUpgradeTimeout, 10*time.Second,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(baseline.Version).NotTo(Equal(inputs.CandidateVersion),
				"candidate version %s is already installed as the GA baseline", baseline.Version)

			By("5. Exercising operator behavior before upgrade")
			Expect(test.BeforeUpgrade(ctx)).To(Succeed())

			By("6. Switching Subscription to candidate CatalogSource")
			_, err = helpers.SwitchSubscriptionCatalog(
				APIClient, spec.SubscriptionName, spec.Namespace, inputs.CatalogName, spec.Channel,
			)
			Expect(err).NotTo(HaveOccurred())

			By("7. Waiting for candidate CSV and version to succeed")
			result, err := helpers.WaitForSubscriptionUpgrade(
				ctx, APIClient, spec, inputs.CatalogName, inputs.CandidateVersion, baseline,
				medik8sparams.OperatorUpgradeTimeout, 10*time.Second,
			)
			Expect(err).NotTo(HaveOccurred())

			By("8. Waiting for controller Deployment to run candidate image")
			if cfg.DeploymentName != "" && cfg.ContainerName != "" {
				Expect(helpers.WaitForDeploymentImage(
					ctx, APIClient, spec.Namespace, cfg.DeploymentName, cfg.ContainerName,
					inputs.CandidateImage, medik8sparams.OperatorUpgradeTimeout, 10*time.Second,
				)).To(Succeed())
			}

			By("9. Proving upgraded operator behavior after upgrade")
			Expect(test.AfterUpgrade(ctx)).To(Succeed())

			AddReportEntry(
				fmt.Sprintf("%s-upgrade-operator-fbc-result", reportPrefix),
				result,
			)
		})
	}

	return Describe(
		fmt.Sprintf("%s Upgrade Operator through FBC", cfg.OperatorName),
		Serial,
		Ordered,
		Label(cfg.Labels...),
		defineFBCUpgradeSuite,
	)
}
