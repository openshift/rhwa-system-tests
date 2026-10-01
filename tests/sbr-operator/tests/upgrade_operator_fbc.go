package tests

import (
	"github.com/medik8s/system-tests/tests/internal/fbcsuite"
	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
)

// Operator-specific SBR lifecycle validation is deferred.
var _ = fbcsuite.DefineFBCUpgradeSuite(fbcsuite.UpgradeOperatorFBCConfig{
	OperatorName:     "SBR",
	PackageName:      sbrparams.UpgradeSBRPackage,
	SubscriptionName: "sbr-operator-upgrade-sub",
	CSVNamePattern:   sbrparams.CSVNamePattern,
	DeploymentName:   sbrparams.OperatorDeploymentName,
	ContainerName:    sbrparams.ManagerContainerName,
	Labels: []string{
		labels.OperatorSBR, sbrparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM,
	},
	PolarionID:                "REPLACE_WITH_POLARION_ID",
	NewUpgradeOperatorFBCTest: fbcsuite.NewStubUpgradeOperatorTest,
})
