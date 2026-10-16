package medik8sparams

import "testing"

func TestExpectedTLSProfilesValue(t *testing.T) {
	testCases := []struct {
		name     string
		major    uint64
		minor    uint64
		expected string
	}{
		{name: "released 0.x bundle", major: 0, minor: 13, expected: "false"},
		{name: "major just below threshold", major: TLSProfilesMinMajorVersion - 1, minor: 99, expected: "false"},
		{
			name: "released NMO 5.7 bundle", major: TLSProfilesMinMajorVersion, minor: TLSProfilesMinMinorVersion - 1,
			expected: "false",
		},
		{
			name: "threshold version", major: TLSProfilesMinMajorVersion, minor: TLSProfilesMinMinorVersion,
			expected: "true",
		},
		{
			name: "minor above threshold", major: TLSProfilesMinMajorVersion, minor: TLSProfilesMinMinorVersion + 1,
			expected: "true",
		},
		{name: "major above threshold", major: TLSProfilesMinMajorVersion + 1, minor: 0, expected: "true"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ExpectedTLSProfilesValue(testCase.major, testCase.minor); got != testCase.expected {
				t.Fatalf("ExpectedTLSProfilesValue(%d, %d) = %q, want %q",
					testCase.major, testCase.minor, got, testCase.expected)
			}
		})
	}
}

func TestExpectedCSVAnnotationsDoesNotMutateInput(t *testing.T) {
	required := map[string]string{"features.operators.openshift.io/disconnected": "true"}
	expected := ExpectedCSVAnnotations(required, TLSProfilesMinMajorVersion, TLSProfilesMinMinorVersion)

	if _, exists := required[TLSProfilesAnnotation]; exists {
		t.Fatalf("input map was mutated with %q", TLSProfilesAnnotation)
	}

	if expected[TLSProfilesAnnotation] != "true" || expected["features.operators.openshift.io/disconnected"] != "true" {
		t.Fatalf("unexpected result: %v", expected)
	}
}
