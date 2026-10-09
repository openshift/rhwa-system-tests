package medik8sparams

import (
	"testing"
)

func TestExpectedFeatureAnnotationsTLSProfiles(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		{"0.13.1", "false"}, // pre-GA bundle
		{"0.3.1", "false"},  // pre-GA bundle
		{"5.8.0", "true"},   // GA bundle
		{"5.0.0", "true"},   // GA boundary
		{"", "false"},       // unparseable -> conservative pre-GA
	}

	for _, testCase := range cases {
		got := ExpectedFeatureAnnotations(testCase.version, true)["features.operators.openshift.io/tls-profiles"]
		if got != testCase.want {
			t.Errorf("version %q: tls-profiles = %q, want %q", testCase.version, got, testCase.want)
		}
	}
}

func TestExpectedFeatureAnnotationsFipsCompliant(t *testing.T) {
	const key = "features.operators.openshift.io/fips-compliant"

	if got := ExpectedFeatureAnnotations("5.8.0", true)[key]; got != "true" {
		t.Errorf("fipsCompliant=true: fips-compliant = %q, want %q", got, "true")
	}

	if got := ExpectedFeatureAnnotations("5.8.0", false)[key]; got != "false" {
		t.Errorf("fipsCompliant=false: fips-compliant = %q, want %q", got, "false")
	}
}
