package tests

import (
	"github.com/medik8s/system-tests/tests/internal/fbcsuite"
	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/mdr-operator/internal/mdrparams"
)

// Operator-specific MDR lifecycle validation is deferred.
var _ = fbcsuite.DefineFBCUpgradeSuite(fbcsuite.UpgradeOperatorFBCConfig{
	OperatorName:     "MDR",
	PackageName:      "machine-deletion-remediation",
	SubscriptionName: "mdr-operator-upgrade-sub",
	CSVNamePattern:   mdrparams.CSVNamePattern,
	DeploymentName:   mdrparams.OperatorDeploymentName,
	ContainerName:    mdrparams.ManagerContainerName,
	Labels: []string{
		labels.OperatorMDR, mdrparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM,
	},
	PolarionID:                "REPLACE_WITH_POLARION_ID",
	NewUpgradeOperatorFBCTest: fbcsuite.NewStubUpgradeOperatorTest,
})
