package medik8sparams

import (
	"strconv"
	"strings"
)

// GA product version at which tls-profiles flipped to "true". All operators
// converged on v5.8.0 for GA; bundles below it are pre-GA and ship "false".
const (
	gaMajor = 5
	gaMinor = 8
)

// ExpectedFeatureAnnotations returns the expected CSV feature-annotation values
// for a given installed bundle version (e.g. the CSV's Spec.Version.String()).
//
// Two annotations are release-dependent, both keyed on the GA version 5.8.0:
//
//   - tls-profiles: pre-GA bundles ship "false", GA bundles (>= 5.8.0) ship
//     "true". A plain major check is not enough: NMO's pre-GA line already used
//     5.x (e.g. v5.7.1 ships "false"), so the threshold is the full GA version.
//   - fips-compliant: "true" for every GA bundle and for every pre-GA bundle
//     except SBR, whose pre-GA line (v0.3.x) shipped "false". Callers pass isSBR
//     so this single exception can be resolved alongside the version.
//
// The same test runs against catalogs of either generation (e.g. the frozen
// rhwa-fbc-422 pre-GA catalog vs. the GA 5.x FBC), so these values are resolved
// from the installed CSV version rather than hardcoded. All other values are
// version-independent.
func ExpectedFeatureAnnotations(csvVersion string, isSBR bool) map[string]string {
	isGA := isGAVersion(csvVersion)
	tlsProfiles := "false"
	if isGA {
		tlsProfiles = "true"
	}

	// fips-compliant is "true" everywhere except the pre-GA SBR bundles.
	fipsCompliant := isGA || !isSBR

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

// isGAVersion reports whether a semver string is at or above the GA product
// version (5.8.0). Unparseable versions are treated as pre-GA, which keeps the
// conservative tls-profiles="false" expectation.
func isGAVersion(version string) bool {
	major, rest, ok := strings.Cut(version, ".")
	if !ok {
		return false
	}

	minor, _, _ := strings.Cut(rest, ".")
	majorN, err := strconv.Atoi(major)
	if err != nil {
		return false
	}

	minorN, err := strconv.Atoi(minor)
	if err != nil {
		return false
	}

	if majorN != gaMajor {
		return majorN > gaMajor
	}

	return minorN >= gaMinor
}
