package medik8sparams

import (
	"strconv"
	"strings"
)

// ExpectedFeatureAnnotations returns the expected CSV feature-annotation values
// for a given installed bundle version (e.g. the CSV's Spec.Version.String()).
//
// The features.operators.openshift.io/tls-profiles value is release-dependent:
// pre-GA bundles (major < 5) ship "false", while GA bundles (major >= 5) ship
// "true". Because the same test runs against catalogs of either generation
// (e.g. the frozen rhwa-fbc-422 pre-GA catalog vs. the GA 5.x FBC), it is
// resolved from the installed CSV version rather than hardcoded.
//
// All other values are version-independent. fipsCompliant differs per operator
// (false for SBR, true for the rest), so it is passed in by the caller.
func ExpectedFeatureAnnotations(csvVersion string, fipsCompliant bool) map[string]string {
	tlsProfiles := "false"
	if majorVersion(csvVersion) >= 5 {
		tlsProfiles = "true"
	}

	return map[string]string{
		"features.operators.openshift.io/tls-profiles":     tlsProfiles,
		"features.operators.openshift.io/disconnected":     "true",
		"features.operators.openshift.io/fips-compliant":   strconv.FormatBool(fipsCompliant),
		"features.operators.openshift.io/proxy-aware":      "false",
		"features.operators.openshift.io/cnf":              "false",
		"features.operators.openshift.io/cni":              "false",
		"features.operators.openshift.io/csi":              "false",
		"features.operators.openshift.io/token-auth-aws":   "false",
		"features.operators.openshift.io/token-auth-azure": "false",
		"features.operators.openshift.io/token-auth-gcp":   "false",
		"operatorframework.io/suggested-namespace":         OperatorNs,
	}
}

// majorVersion extracts the leading major-version integer from a semver string
// (e.g. "5.8.0" -> 5, "0.13.1" -> 0). Returns 0 when the version cannot be
// parsed, which keeps the conservative pre-GA expectation.
func majorVersion(version string) int {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}

	return n
}
