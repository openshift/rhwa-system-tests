package medik8sparams

import (
	"testing"
)

func TestExpectedFeatureAnnotationsTLSProfiles(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		{"0.13.1", "false"}, // pre-GA bundle (SNR)
		{"0.3.1", "false"},  // pre-GA bundle (SBR)
		{"5.7.1", "false"},  // pre-GA NMO already on 5.x
		{"5.8.0", "true"},   // GA bundle
		{"5.8.2", "true"},   // GA z-stream
		{"5.10.0", "true"},  // later GA minor (string compare would misorder)
		{"6.0.0", "true"},   // later GA major
		{"", "false"},       // unparseable -> conservative pre-GA
	}

	for _, testCase := range cases {
		// isSBR is irrelevant to tls-profiles; pass false.
		got := ExpectedFeatureAnnotations(testCase.version, false)["features.operators.openshift.io/tls-profiles"]
		if got != testCase.want {
			t.Errorf("version %q: tls-profiles = %q, want %q", testCase.version, got, testCase.want)
		}
	}
}

func TestExpectedFeatureAnnotationsFipsCompliant(t *testing.T) {
	const key = "features.operators.openshift.io/fips-compliant"

	cases := []struct {
		version string
		isSBR   bool
		want    string
	}{
		{"5.8.0", false, "true"}, // GA non-SBR
		{"5.8.0", true, "true"},  // GA SBR (fips flipped to true at GA)
		{"0.8.1", false, "true"}, // pre-GA non-SBR (FAR)
		{"0.3.1", true, "false"}, // pre-GA SBR -- the only "false" case
		{"5.7.1", false, "true"}, // pre-GA NMO (non-SBR)
	}

	for _, testCase := range cases {
		got := ExpectedFeatureAnnotations(testCase.version, testCase.isSBR)[key]
		if got != testCase.want {
			t.Errorf("version %q isSBR=%v: fips-compliant = %q, want %q",
				testCase.version, testCase.isSBR, got, testCase.want)
		}
	}
}
