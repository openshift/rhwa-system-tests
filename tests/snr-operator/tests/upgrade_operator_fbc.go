package tests

import (
	"github.com/medik8s/system-tests/tests/internal/fbcsuite"
	"github.com/medik8s/system-tests/tests/internal/labels"
	"github.com/medik8s/system-tests/tests/snr-operator/internal/snrparams"
)

// Operator-specific SNR lifecycle validation is deferred.
var _ = fbcsuite.DefineFBCUpgradeSuite(fbcsuite.UpgradeOperatorFBCConfig{
	OperatorName:     "SNR",
	PackageName:      "self-node-remediation",
	SubscriptionName: "snr-operator-upgrade-sub",
	CSVNamePattern:   snrparams.CSVNamePattern,
	DeploymentName:   snrparams.OperatorDeploymentName,
	ContainerName:    snrparams.ManagerContainerName,
	Labels: []string{
		labels.OperatorSNR, snrparams.Label, labels.TierUpgradeOperator,
		labels.DisruptionNonDestructive, labels.PlatformAny, labels.ComponentOLM,
	},
	PolarionID:                "REPLACE_WITH_POLARION_ID",
	NewUpgradeOperatorFBCTest: fbcsuite.NewStubUpgradeOperatorTest,
})
