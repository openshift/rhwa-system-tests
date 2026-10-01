package nhcutils

import (
	"fmt"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
	olmV1alpha1 "github.com/rh-ecosystem-edge/eco-goinfra/pkg/schemes/olm/operators/v1alpha1"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/nhc-operator/internal/nhcparams"
)

// CreateCandidateCatalog creates the test-owned catalog for a candidate operator upgrade.
func CreateCandidateCatalog(apiClient *clients.Settings, image string) (*olm.CatalogSourceBuilder, error) {
	catalog := olm.NewCatalogSourceBuilder(
		apiClient, nhcparams.CandidateCatalogName, medik8sparams.GACatalogNamespace)
	if catalog == nil {
		return nil, fmt.Errorf("create candidate CatalogSource builder")
	}

	catalog.Definition.Spec.SourceType = olmV1alpha1.SourceTypeGrpc
	catalog.Definition.Spec.Image = image
	catalog.Definition.Spec.DisplayName = "NHC upgrade candidate"
	catalog.Definition.Spec.Publisher = "medik8s system-tests"

	return catalog.Create()
}

// DeleteCandidateCatalog deletes only the CatalogSource with the test-owned fixed name.
func DeleteCandidateCatalog(apiClient *clients.Settings) error {
	catalog := olm.NewCatalogSourceBuilder(
		apiClient, nhcparams.CandidateCatalogName, medik8sparams.GACatalogNamespace)
	if catalog == nil {
		return fmt.Errorf("create candidate CatalogSource builder")
	}

	return catalog.Delete()
}

// InstallGAOperator creates a Subscription for the NHC operator from the built-in
// redhat-operators catalog on the current OCP cluster.
func InstallGAOperator(apiClient *clients.Settings) (*olm.SubscriptionBuilder, error) {
	return helpers.InstallGAOperatorSubscription(
		apiClient,
		nhcparams.ClusterUpgradeSubName,
		medik8sparams.OperatorNs,
		medik8sparams.GAOperatorCatalog,
		medik8sparams.GACatalogNamespace,
		nhcparams.UpgradeNHCPackage,
		medik8sparams.GAChannel,
	)
}

// InstallGASNR installs the released SNR prerequisite from the same built-in catalog.
func InstallGASNR(apiClient *clients.Settings) (*olm.SubscriptionBuilder, error) {
	return helpers.InstallGAOperatorSubscription(
		apiClient,
		nhcparams.ClusterUpgradeSNRSubName,
		medik8sparams.OperatorNs,
		medik8sparams.GAOperatorCatalog,
		medik8sparams.GACatalogNamespace,
		nhcparams.UpgradeSNRPackage,
		medik8sparams.GAChannel,
	)
}

// SwitchSubscriptionCatalog updates the upgrade-test Subscription to point to the
// given CatalogSource name and target channel.
func SwitchSubscriptionCatalog(
	apiClient *clients.Settings, catalogName string,
) (*olm.SubscriptionBuilder, error) {
	return helpers.SwitchSubscriptionCatalog(
		apiClient,
		nhcparams.ClusterUpgradeSubName,
		medik8sparams.OperatorNs,
		catalogName,
		medik8sparams.TargetChannel,
	)
}

// CleanupUpgradeResources removes the Subscription created during the upgrade test.
func CleanupUpgradeResources(apiClient *clients.Settings, logf func(string, ...interface{})) {
	helpers.DeleteSubscription(apiClient, nhcparams.ClusterUpgradeSubName, medik8sparams.OperatorNs, logf)
	helpers.DeleteStaleCSVsAndInstallPlans(
		apiClient, nhcparams.CSVNamePattern, medik8sparams.OperatorNs, logf)
	helpers.DeleteSubscription(apiClient, nhcparams.ClusterUpgradeSNRSubName, medik8sparams.OperatorNs, logf)
	helpers.DeleteStaleCSVsAndInstallPlans(
		apiClient, nhcparams.ClusterUpgradeSNRCSVPattern, medik8sparams.OperatorNs, logf)
}
