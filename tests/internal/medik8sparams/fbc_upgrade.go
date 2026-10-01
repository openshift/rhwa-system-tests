package medik8sparams

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FBCUpgradeInputs are the release artifacts for one operator FBC upgrade scenario.
type FBCUpgradeInputs struct {
	CatalogImage     string `json:"catalogImage"`
	CatalogName      string `json:"catalogName"`
	IDMSPath         string `json:"idmsPath"`
	CandidateVersion string `json:"candidateVersion"`
	CandidateImage   string `json:"candidateImage"`
	Channel          string `json:"channel"`
	SkipCleanup      bool   `json:"skipCleanup"`
}

// LoadFBCUpgradeInputs reads and validates one operator's FBC upgrade contract.
func LoadFBCUpgradeInputs(operatorName string) (FBCUpgradeInputs, error) {
	operatorName = strings.ToUpper(strings.TrimSpace(operatorName))
	if operatorName == "" {
		return FBCUpgradeInputs{}, fmt.Errorf("operator name must be set for FBC upgrade tests")
	}

	envName := func(suffix string) string {
		return operatorName + "_FBC_" + suffix
	}

	inputs := FBCUpgradeInputs{
		CatalogImage:     strings.TrimSpace(os.Getenv(envName("CATALOG_IMAGE"))),
		CatalogName:      envOrDefault(envName("CATALOG_NAME"), strings.ToLower(operatorName)+"-upgrade-candidate"),
		IDMSPath:         strings.TrimSpace(os.Getenv(envName("IDMS_PATH"))),
		CandidateVersion: strings.TrimSpace(os.Getenv(envName("CANDIDATE_VERSION"))),
		CandidateImage:   strings.TrimSpace(os.Getenv(envName("CANDIDATE_IMAGE"))),
		Channel:          envOrDefault(envName("CHANNEL"), GAChannel),
	}

	if sharedDir := strings.TrimSpace(os.Getenv("SHARED_DIR")); inputs.IDMSPath == "" && sharedDir != "" {
		sharedIDMSPath := filepath.Join(sharedDir, "idms.yaml")
		if _, err := os.Stat(sharedIDMSPath); err == nil {
			inputs.IDMSPath = sharedIDMSPath
		}
	}

	for key, value := range map[string]string{
		envName("CATALOG_IMAGE"):     inputs.CatalogImage,
		envName("CANDIDATE_VERSION"): inputs.CandidateVersion,
		envName("CANDIDATE_IMAGE"):   inputs.CandidateImage,
	} {
		if value == "" {
			return FBCUpgradeInputs{}, fmt.Errorf("%s must be set for FBC upgrade tests", key)
		}
	}

	if !isDigestPinned(inputs.CatalogImage) {
		return FBCUpgradeInputs{}, fmt.Errorf(
			"%s must be digest pinned with @sha256", envName("CATALOG_IMAGE"))
	}

	if !isDigestPinned(inputs.CandidateImage) {
		return FBCUpgradeInputs{}, fmt.Errorf(
			"%s must be digest pinned with @sha256", envName("CANDIDATE_IMAGE"))
	}

	skipCleanupName := envName("SKIP_CLEANUP")

	skipCleanup := strings.TrimSpace(os.Getenv(skipCleanupName))
	switch skipCleanup {
	case "", "false":
	case "true":
		inputs.SkipCleanup = true
	default:
		return FBCUpgradeInputs{}, fmt.Errorf(
			"%s must be true or false, got %q", skipCleanupName, skipCleanup)
	}

	return inputs, nil
}

func isDigestPinned(image string) bool {
	name, digest, found := strings.Cut(image, "@sha256:")

	return found && name != "" && len(digest) == 64 &&
		strings.Trim(digest, "0123456789abcdef") == ""
}
