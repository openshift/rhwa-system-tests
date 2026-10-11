package medik8sparams

import "maps"

const (
	// TLSProfilesAnnotation is the CSV feature annotation advertising TLS security profile support.
	TLSProfilesAnnotation = "features.operators.openshift.io/tls-profiles"

	// TLSProfilesMinMajorVersion is the major part of the first operator version (5.8) whose bundle sets
	// tls-profiles to "true". Released 0.x bundles (FAR, MDR, NHC, SBR, SNR) and released NMO 5.x bundles
	// before 5.8 (NMO already used 5.x versioning, e.g. 5.7.1) set it to "false".
	TLSProfilesMinMajorVersion = 5

	// TLSProfilesMinMinorVersion is the minor part of the first operator version (5.8) whose bundle sets
	// tls-profiles to "true".
	TLSProfilesMinMinorVersion = 8
)

// ExpectedTLSProfilesValue returns the expected tls-profiles annotation value for an operator CSV version.
func ExpectedTLSProfilesValue(major, minor uint64) string {
	if major > TLSProfilesMinMajorVersion ||
		(major == TLSProfilesMinMajorVersion && minor >= TLSProfilesMinMinorVersion) {
		return "true"
	}

	return "false"
}

// ExpectedCSVAnnotations returns a copy of the version-independent required annotations with the
// version-dependent tls-profiles expectation added for the given CSV major and minor version.
func ExpectedCSVAnnotations(required map[string]string, major, minor uint64) map[string]string {
	expected := maps.Clone(required)
	if expected == nil {
		expected = map[string]string{}
	}

	expected[TLSProfilesAnnotation] = ExpectedTLSProfilesValue(major, minor)

	return expected
}
