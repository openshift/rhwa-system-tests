package tests

import (
	"github.com/medik8s/system-tests/tests/internal/fbcsuite"
	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/nmo-operator/internal/nmoparams"
)

// Operator-specific NMO lifecycle validation is deferred.
var _ = fbcsuite.DefineFBCUpgradeSuite(fbcsuite.UpgradeOperatorFBCConfig{
	OperatorName:     "NMO",
	PackageName:      "node-maintenance-operator",
	SubscriptionName: "nmo-operator-upgrade-sub",
	CSVNamePattern:   nmoparams.CSVNamePattern,
	DeploymentName:   nmoparams.OperatorDeploymentName,
	ContainerName:    nmoparams.ManagerContainerName,
	Labels: []string{
		labels.OperatorNMO, nmoparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM,
	},
	PolarionID:                "REPLACE_WITH_POLARION_ID",
	NewUpgradeOperatorFBCTest: fbcsuite.NewStubUpgradeOperatorTest,
})
