package farparams

import (
	"testing"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
)

func TestRequiredAnnotationsExcludesTLSProfiles(t *testing.T) {
	if _, exists := RequiredAnnotations[medik8sparams.TLSProfilesAnnotation]; exists {
		t.Fatalf("RequiredAnnotations must not contain %q; it is version-dependent", medik8sparams.TLSProfilesAnnotation)
	}
}
