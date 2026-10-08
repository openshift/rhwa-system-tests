package farparams

import "testing"

func TestExpectedTLSProfilesValue(t *testing.T) {
	testCases := []struct {
		name     string
		major    uint64
		expected string
	}{
		{name: "released 0.x bundle", major: 0, expected: "false"},
		{name: "major just below threshold", major: TLSProfilesMinMajorVersion - 1, expected: "false"},
		{name: "threshold major", major: TLSProfilesMinMajorVersion, expected: "true"},
		{name: "major above threshold", major: TLSProfilesMinMajorVersion + 1, expected: "true"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ExpectedTLSProfilesValue(testCase.major); got != testCase.expected {
				t.Fatalf("ExpectedTLSProfilesValue(%d) = %q, want %q", testCase.major, got, testCase.expected)
			}
		})
	}
}

func TestRequiredAnnotationsExcludesTLSProfiles(t *testing.T) {
	if _, exists := RequiredAnnotations[TLSProfilesAnnotation]; exists {
		t.Fatalf("RequiredAnnotations must not contain %q; it is version-dependent", TLSProfilesAnnotation)
	}
}
