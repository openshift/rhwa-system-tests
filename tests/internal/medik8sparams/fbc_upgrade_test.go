package medik8sparams

import (
	"strings"
	"testing"
)

func TestLoadFBCUpgradeInputs(t *testing.T) {
	t.Setenv("SHARED_DIR", t.TempDir())
	t.Setenv("NHC_FBC_CATALOG_IMAGE", "quay.io/example/catalog@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NHC_FBC_CANDIDATE_VERSION", "5.8.0")
	t.Setenv("NHC_FBC_CANDIDATE_IMAGE", "quay.io/example/operator@sha256:"+strings.Repeat("b", 64))

	inputs, err := LoadFBCUpgradeInputs("NHC")
	if err != nil {
		t.Fatalf("LoadFBCUpgradeInputs returned an error: %v", err)
	}

	if inputs.CatalogName != "nhc-upgrade-candidate" || inputs.Channel != GAChannel {
		t.Fatalf("unexpected defaults: %#v", inputs)
	}

	if inputs.IDMSPath != "" {
		t.Fatalf("unexpected IDMS path: %s", inputs.IDMSPath)
	}
}

func TestLoadFBCUpgradeInputsRequiresDigestPinnedCatalog(t *testing.T) {
	t.Setenv("NHC_FBC_CATALOG_IMAGE", "quay.io/example/catalog:latest")
	t.Setenv("NHC_FBC_IDMS_PATH", "/tmp/idms.yaml")
	t.Setenv("NHC_FBC_CANDIDATE_VERSION", "5.8.0")
	t.Setenv("NHC_FBC_CANDIDATE_IMAGE", "quay.io/example/operator@sha256:"+strings.Repeat("b", 64))

	if _, err := LoadFBCUpgradeInputs("NHC"); err == nil || !strings.Contains(err.Error(), "digest pinned") {
		t.Fatalf("expected digest pin validation error, got %v", err)
	}
}

func TestLoadFBCUpgradeInputsRequiresDigestPinnedCandidate(t *testing.T) {
	t.Setenv("NHC_FBC_CATALOG_IMAGE", "quay.io/example/catalog@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NHC_FBC_IDMS_PATH", "/tmp/idms.yaml")
	t.Setenv("NHC_FBC_CANDIDATE_VERSION", "5.8.0")
	t.Setenv("NHC_FBC_CANDIDATE_IMAGE", "quay.io/example/operator:latest")

	if _, err := LoadFBCUpgradeInputs("NHC"); err == nil || !strings.Contains(err.Error(), "CANDIDATE_IMAGE") {
		t.Fatalf("expected candidate digest pin validation error, got %v", err)
	}
}

func TestLoadFBCUpgradeInputsRequiresAllArtifacts(t *testing.T) {
	for _, variable := range []string{
		"NHC_FBC_CATALOG_IMAGE",
		"NHC_FBC_CANDIDATE_VERSION",
		"NHC_FBC_CANDIDATE_IMAGE",
	} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv("NHC_FBC_CATALOG_IMAGE", "quay.io/example/catalog@sha256:"+strings.Repeat("a", 64))
			t.Setenv("NHC_FBC_IDMS_PATH", "/tmp/idms.yaml")
			t.Setenv("NHC_FBC_CANDIDATE_VERSION", "5.8.0")
			t.Setenv("NHC_FBC_CANDIDATE_IMAGE", "quay.io/example/operator@sha256:"+strings.Repeat("b", 64))
			t.Setenv(variable, "")

			if _, err := LoadFBCUpgradeInputs("NHC"); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("expected missing %s error, got %v", variable, err)
			}
		})
	}
}

func TestLoadFBCUpgradeInputsRejectsInvalidSkipCleanup(t *testing.T) {
	t.Setenv("NHC_FBC_CATALOG_IMAGE", "quay.io/example/catalog@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NHC_FBC_IDMS_PATH", "/tmp/idms.yaml")
	t.Setenv("NHC_FBC_CANDIDATE_VERSION", "5.8.0")
	t.Setenv("NHC_FBC_CANDIDATE_IMAGE", "quay.io/example/operator@sha256:"+strings.Repeat("b", 64))
	t.Setenv("NHC_FBC_SKIP_CLEANUP", "yes")

	if _, err := LoadFBCUpgradeInputs("NHC"); err == nil || !strings.Contains(err.Error(), "true or false") {
		t.Fatalf("expected boolean validation error, got %v", err)
	}
}

func TestLoadFBCUpgradeInputsSeparatesOperators(t *testing.T) {
	t.Setenv("NHC_FBC_CATALOG_IMAGE", "quay.io/example/nhc-catalog@sha256:"+strings.Repeat("a", 64))
	t.Setenv("NHC_FBC_CANDIDATE_VERSION", "5.8.0")
	t.Setenv("NHC_FBC_CANDIDATE_IMAGE", "quay.io/example/nhc@sha256:"+strings.Repeat("b", 64))
	t.Setenv("SBR_FBC_CATALOG_IMAGE", "quay.io/example/sbr-catalog@sha256:"+strings.Repeat("c", 64))
	t.Setenv("SBR_FBC_CANDIDATE_VERSION", "5.9.0")
	t.Setenv("SBR_FBC_CANDIDATE_IMAGE", "quay.io/example/sbr@sha256:"+strings.Repeat("d", 64))

	nhcInputs, err := LoadFBCUpgradeInputs("nhc")
	if err != nil {
		t.Fatalf("load NHC inputs: %v", err)
	}

	sbrInputs, err := LoadFBCUpgradeInputs("SBR")
	if err != nil {
		t.Fatalf("load SBR inputs: %v", err)
	}

	if nhcInputs.CandidateVersion != "5.8.0" || sbrInputs.CandidateVersion != "5.9.0" {
		t.Fatalf("operator inputs overlapped: NHC=%#v SBR=%#v", nhcInputs, sbrInputs)
	}
}
