package nhcparams

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
)

const (
	// UpgradeSubName selects the NHC upgrade scenario.
	UpgradeSubName = "nhc-operator-upgrade"
	// NHCUpgradeTestName is the fixed name of the test-owned NodeHealthCheck.
	NHCUpgradeTestName = "nhc-operator-upgrade"
	// NHCUpgradeTemplateName is the fixed name of the test-owned remediation template.
	NHCUpgradeTemplateName = "nhc-operator-upgrade-template"
	// ClusterUpgradeSubName is the Subscription used by the full OCP-and-operator upgrade test.
	ClusterUpgradeSubName = "nhc-upgrade-sub"
	// ClusterUpgradeTestName is the NodeHealthCheck used by the full OCP-and-operator upgrade test.
	ClusterUpgradeTestName = "nhc-upgrade-test"
	// ClusterUpgradeSNRSubName is the test-owned SNR prerequisite Subscription.
	ClusterUpgradeSNRSubName = "nhc-upgrade-snr"
	// UpgradeSNRPackage is the released SNR package installed as the remediator.
	UpgradeSNRPackage = "self-node-remediation"
	// ClusterUpgradeSNRCSVPattern identifies the released SNR CSV.
	ClusterUpgradeSNRCSVPattern = "self-node-remediation"
	// ClusterUpgradeSNRDeploymentName identifies the released SNR controller.
	ClusterUpgradeSNRDeploymentName = "self-node-remediation-controller-manager"
	// ClusterUpgradeSNRWebhookServiceName identifies the SNR admission webhook Service.
	ClusterUpgradeSNRWebhookServiceName = "self-node-remediation-controller-manager-service"
	// UpgradeNHCPackage is the fixed NHC package under test.
	UpgradeNHCPackage = "node-healthcheck-operator"
	// UpgradeNamespace is the established namespace shared by the NHC and SNR operators.
	UpgradeNamespace = medik8sparams.OperatorNs
	// BaselineNHCBundleRepository contains released downstream NHC bundles.
	BaselineNHCBundleRepository = "registry.redhat.io/workload-availability/node-healthcheck-operator-bundle"
	// BaselineSNRBundleRepository contains released downstream SNR bundles.
	BaselineSNRBundleRepository = "registry.redhat.io/workload-availability/self-node-remediation-operator-bundle"
	// CandidateCatalogName is the CatalogSource owned by the PR-candidate cluster test.
	CandidateCatalogName = "nhc-upgrade-candidate"
	// UpgradeRemediationCompletionTimeout bounds each destructive remediation checkpoint.
	UpgradeRemediationCompletionTimeout = 20 * time.Minute
)

// OperatorArtifact identifies one bundle and the operator version and image it contains.
type OperatorArtifact struct {
	Bundle  string `json:"bundle"`
	Version string `json:"version"`
	Image   string `json:"image"`
}

// UpgradeOperatorInputs are the artifacts used by tier:upgrade-operator.
type UpgradeOperatorInputs struct {
	BaselineNHC  OperatorArtifact `json:"baselineNHC"`
	CandidateNHC OperatorArtifact `json:"candidateNHC"`
	BaselineSNR  OperatorArtifact `json:"baselineSNR"`
	CandidateSNR OperatorArtifact `json:"candidateSNR"`
	TestRevision string           `json:"testRevision"`
	Package      string           `json:"package"`
	SNRPackage   string           `json:"snrPackage"`
	Namespace    string           `json:"namespace"`
	OperatorSDK  string           `json:"operatorSDK"`
	SkipCleanup  bool             `json:"skipCleanup"`
}

// FreshInstallInputs are the artifacts used by tier:fresh-install.
type FreshInstallInputs struct {
	CandidateNHC OperatorArtifact `json:"candidateNHC"`
	CandidateSNR OperatorArtifact `json:"candidateSNR"`
	TestRevision string           `json:"testRevision"`
	Package      string           `json:"package"`
	SNRPackage   string           `json:"snrPackage"`
	Namespace    string           `json:"namespace"`
	OperatorSDK  string           `json:"operatorSDK"`
	SkipCleanup  bool             `json:"skipCleanup"`
}

// UpgradeClusterInputs are the PR-built artifacts used by tier:upgrade-cluster.
type UpgradeClusterInputs struct {
	CandidateNHC     OperatorArtifact `json:"candidateNHC"`
	CandidateCatalog string           `json:"candidateCatalog"`
	TestRevision     string           `json:"testRevision"`
	Package          string           `json:"package"`
	Namespace        string           `json:"namespace"`
}

// LoadUpgradeOperatorInputs reads candidate inputs and optional baseline/SNR overrides.
func LoadUpgradeOperatorInputs() (UpgradeOperatorInputs, error) {
	skipCleanup, err := loadSkipCleanup()
	if err != nil {
		return UpgradeOperatorInputs{}, err
	}

	testRevision, err := loadTestRevision()
	if err != nil {
		return UpgradeOperatorInputs{}, err
	}

	candidateNHC, operatorSDK, err := loadCandidateNHC("NHC operator upgrade scenario")
	if err != nil {
		return UpgradeOperatorInputs{}, err
	}

	return UpgradeOperatorInputs{
		BaselineNHC: OperatorArtifact{
			Bundle:  os.Getenv("NHC_UPGRADE_BASELINE_NHC_BUNDLE"),
			Version: os.Getenv("NHC_UPGRADE_BASELINE_NHC_VERSION"),
			Image:   os.Getenv("NHC_UPGRADE_BASELINE_NHC_IMAGE"),
		},
		CandidateNHC: candidateNHC,
		BaselineSNR: OperatorArtifact{
			Bundle:  os.Getenv("NHC_UPGRADE_BASELINE_SNR_BUNDLE"),
			Version: os.Getenv("NHC_UPGRADE_BASELINE_SNR_VERSION"),
			Image:   os.Getenv("NHC_UPGRADE_BASELINE_SNR_IMAGE"),
		},
		CandidateSNR: OperatorArtifact{
			Bundle:  os.Getenv("NHC_UPGRADE_CANDIDATE_SNR_BUNDLE"),
			Version: os.Getenv("NHC_UPGRADE_CANDIDATE_SNR_VERSION"),
			Image:   os.Getenv("NHC_UPGRADE_CANDIDATE_SNR_IMAGE"),
		},
		TestRevision: testRevision, Package: UpgradeNHCPackage, SNRPackage: UpgradeSNRPackage,
		Namespace: UpgradeNamespace, OperatorSDK: operatorSDK, SkipCleanup: skipCleanup,
	}, nil
}

// LoadFreshInstallInputs reads candidate NHC and SNR artifacts for a fresh installation.
func LoadFreshInstallInputs() (FreshInstallInputs, error) {
	skipCleanup, err := loadSkipCleanup()
	if err != nil {
		return FreshInstallInputs{}, err
	}

	testRevision, err := loadTestRevision()
	if err != nil {
		return FreshInstallInputs{}, err
	}

	candidateNHC, operatorSDK, err := loadCandidateNHC("NHC fresh-install scenario")
	if err != nil {
		return FreshInstallInputs{}, err
	}

	return FreshInstallInputs{
		CandidateNHC: candidateNHC,
		CandidateSNR: OperatorArtifact{
			Bundle:  os.Getenv("NHC_UPGRADE_CANDIDATE_SNR_BUNDLE"),
			Version: os.Getenv("NHC_UPGRADE_CANDIDATE_SNR_VERSION"),
			Image:   os.Getenv("NHC_UPGRADE_CANDIDATE_SNR_IMAGE"),
		},
		TestRevision: testRevision, Package: UpgradeNHCPackage, SNRPackage: UpgradeSNRPackage,
		Namespace: UpgradeNamespace, OperatorSDK: operatorSDK, SkipCleanup: skipCleanup,
	}, nil
}

// LoadUpgradeClusterInputs reads the PR-built NHC artifact used after a real OpenShift upgrade.
func LoadUpgradeClusterInputs() (UpgradeClusterInputs, error) {
	testRevision, err := loadTestRevision()
	if err != nil {
		return UpgradeClusterInputs{}, err
	}

	candidateNHC := candidateNHCFromEnvironment()

	candidateCatalog := os.Getenv("NHC_UPGRADE_CANDIDATE_NHC_CATALOG")
	for key, value := range map[string]string{
		"NHC_UPGRADE_CANDIDATE_NHC_BUNDLE":  candidateNHC.Bundle,
		"NHC_UPGRADE_CANDIDATE_NHC_VERSION": candidateNHC.Version,
		"NHC_UPGRADE_CANDIDATE_NHC_IMAGE":   candidateNHC.Image,
		"NHC_UPGRADE_CANDIDATE_NHC_CATALOG": candidateCatalog,
	} {
		if value == "" {
			return UpgradeClusterInputs{}, fmt.Errorf("%s must be set for the NHC candidate cluster-upgrade scenario", key)
		}
	}

	return UpgradeClusterInputs{
		CandidateNHC: candidateNHC, CandidateCatalog: candidateCatalog, TestRevision: testRevision,
		Package: UpgradeNHCPackage, Namespace: UpgradeNamespace,
	}, nil
}

func loadCandidateNHC(scenario string) (OperatorArtifact, string, error) {
	// These four caller-supplied values may be populated by Makefile automation in the future.
	candidate := candidateNHCFromEnvironment()
	operatorSDK := os.Getenv("NHC_UPGRADE_OPERATOR_SDK")

	for key, value := range map[string]string{
		"NHC_UPGRADE_CANDIDATE_NHC_BUNDLE":  candidate.Bundle,
		"NHC_UPGRADE_CANDIDATE_NHC_VERSION": candidate.Version,
		"NHC_UPGRADE_CANDIDATE_NHC_IMAGE":   candidate.Image,
		"NHC_UPGRADE_OPERATOR_SDK":          operatorSDK,
	} {
		if value == "" {
			return OperatorArtifact{}, "", fmt.Errorf("%s must be set for the %s", key, scenario)
		}
	}

	return candidate, operatorSDK, nil
}

func candidateNHCFromEnvironment() OperatorArtifact {
	return OperatorArtifact{
		Bundle:  os.Getenv("NHC_UPGRADE_CANDIDATE_NHC_BUNDLE"),
		Version: os.Getenv("NHC_UPGRADE_CANDIDATE_NHC_VERSION"),
		Image:   os.Getenv("NHC_UPGRADE_CANDIDATE_NHC_IMAGE"),
	}
}

func loadSkipCleanup() (bool, error) {
	skipCleanup, err := strconv.ParseBool(envOrDefault("NHC_UPGRADE_SKIP_CLEANUP", "false"))
	if err != nil {
		return false, fmt.Errorf("NHC_UPGRADE_SKIP_CLEANUP must be a boolean: %w", err)
	}

	return skipCleanup, nil
}

func loadTestRevision() (string, error) {
	revision := os.Getenv("NHC_UPGRADE_TEST_REVISION")
	if revision == "" {
		output, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			return "", fmt.Errorf("derive NHC_UPGRADE_TEST_REVISION: %w", err)
		}

		revision = strings.TrimSpace(string(output))
	}

	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) {
		return "", fmt.Errorf("NHC_UPGRADE_TEST_REVISION must be a full Git commit hash")
	}

	return revision, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
