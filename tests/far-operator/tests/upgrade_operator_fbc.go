package tests

import (
	"github.com/medik8s/system-tests/tests/far-operator/internal/farparams"
	"github.com/medik8s/system-tests/tests/internal/fbcsuite"
	"github.com/medik8s/system-tests/tests/internal/labels"
)

// Operator-specific FAR lifecycle validation is deferred.
var _ = fbcsuite.DefineFBCUpgradeSuite(fbcsuite.UpgradeOperatorFBCConfig{
	OperatorName:     "FAR",
	PackageName:      "fence-agents-remediation",
	SubscriptionName: farparams.UpgradeSubName,
	CSVNamePattern:   "fence-agents-remediation",
	DeploymentName:   farparams.OperatorDeploymentName,
	ContainerName:    farparams.ManagerContainerName,
	Labels: []string{
		labels.OperatorFAR, farparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM,
	},
	PolarionID:                "REPLACE_WITH_POLARION_ID",
	NewUpgradeOperatorFBCTest: fbcsuite.NewStubUpgradeOperatorTest,
})
