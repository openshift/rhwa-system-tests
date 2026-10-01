package tests

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/medik8s/system-tests/tests/internal/fbcsuite"
	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcutils"
)

func prepareGASNR(ctx context.Context) error {
	if _, err := nhcutils.InstallGASNR(APIClient); err != nil {
		return fmt.Errorf("create GA SNR subscription: %w", err)
	}

	if _, err := helpers.WaitForInstalledOperator(ctx, APIClient, helpers.OperatorOLMSpec{
		Package:          nhcparams.UpgradeSNRPackage,
		SubscriptionName: nhcparams.ClusterUpgradeSNRSubName,
		Namespace:        medik8sparams.OperatorNs,
		CSVNamePattern:   nhcparams.ClusterUpgradeSNRCSVPattern,
		Channel:          medik8sparams.GAChannel,
	}, medik8sparams.OperatorUpgradeTimeout, nhcparams.DefaultPollInterval); err != nil {
		return fmt.Errorf("wait for GA SNR CSV: %w", err)
	}

	if err := waitForSNRControllerAndWebhook(ctx); err != nil {
		return fmt.Errorf("wait for GA SNR controller and webhook: %w", err)
	}

	return nil
}

func newNHCFBCTest(medik8sparams.FBCUpgradeInputs) fbcsuite.UpgradeOperatorFBCTest {
	owned := &nhcutils.OwnedRun{
		API: APIClient, Namespace: medik8sparams.OperatorNs, Token: rand.Text(),
	}

	return &nhcUpgradeOperatorFBCTest{
		owned:      owned,
		namespace:  medik8sparams.OperatorNs,
		token:      owned.Token,
		prepareSNR: prepareGASNR,
	}
}

var _ = fbcsuite.DefineFBCUpgradeSuite(fbcsuite.UpgradeOperatorFBCConfig{
	OperatorName:     "NHC",
	PackageName:      nhcparams.UpgradeNHCPackage,
	SubscriptionName: nhcparams.ClusterUpgradeSubName,
	CSVNamePattern:   nhcparams.CSVNamePattern,
	DeploymentName:   nhcparams.OperatorDeploymentName,
	ContainerName:    nhcparams.ManagerContainerName,
	Labels: []string{
		labels.OperatorNHC, nhcparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionDestructive, labels.PlatformAny, labels.ComponentOLM,
		labels.ComponentRemediation,
	},
	PolarionID:                "REPLACE_WITH_POLARION_ID",
	NewUpgradeOperatorFBCTest: newNHCFBCTest,
})
