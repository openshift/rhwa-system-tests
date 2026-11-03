#!/usr/bin/env bash

set -e

. "$(dirname "$0")"/common.sh

# 2.15.0 is the first release whose go/packages can read the export data Go 1.27.2 writes.
# Older pins fail every package with "export data version 5 is greater than maximum supported
# version 4" on a 1.27.2 toolchain.
GOLANGCI_LINT_VERSION="2.15.0"

# GOLANGCI_LINT_BIN is the executable every function below uses. It starts as the one on $PATH and
# becomes the absolute path of the binary this script installs, so an unrelated golangci-lint
# earlier on $PATH cannot shadow the pinned version and run the lint with the wrong linter set.
GOLANGCI_LINT_BIN="golangci-lint"

# IsGoLangCiLintInstalled is used to check whether golangci-lint executable is on the $PATH.
function IsGolangCiLintInstalled() {

	echo "Checking if golangci-lint is installed"

	if which golangci-lint &> /dev/null; then
		return 0
	fi

	echo "Could not find golangci-lint on path"

	return 1
}

# IsGolangCiLintCorrectVersion is used to check the installed version of golangci-lint and ensure it is as expected.
function IsGolangCiLintCorrectVersion() {

	local requiredVersion
	requiredVersion="${1}"

	local installedVersion
	installedVersion="$("${GOLANGCI_LINT_BIN}" version 2> /dev/null | awk '{ print $4 }')"
	echo "Checking if installed golang version matches requirement"

	if [[ "${installedVersion}" == "${requiredVersion}" ]]; then
		return 0
	fi

	echo "Installed version does not match expected version"

	return 1
}

# DownloadGolangCiLint is used to download a specified version of golangci-lint
function DownloadGolangCiLint() {

	local versionNumber
	versionNumber="${1}"

	local installDir
	installDir="$(go env GOPATH)/bin"

	echo "installing golangci-lint version ${versionNumber}"
	if curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/main/install.sh | sh -s -- -b "${installDir}" v${versionNumber}; then
		# Call the binary we just installed, not whichever one $PATH resolves first.
		GOLANGCI_LINT_BIN="${installDir}/golangci-lint"

		return 0
	fi

	return 1
}

# RunGolangCiLint is used to execute the requested golangci-lint command
function RunGolangCiLint() {

	echo "Running golangci-lint (${GOLANGCI_LINT_BIN})"

	if "${GOLANGCI_LINT_BIN}" "$@"; then
		return 0;
	fi

	return 1
}

# Main body of shell script
function Main() {

	if [[ $# -eq 0 ]]; then
		set -- run -v
	fi

	# Check whether we need to install golangci-lint
	local installRequired
	installRequired="false"

	# Check if its present at all
	if ! IsGolangCiLintInstalled; then
		installRequired="true"
	fi

	# Check if its the right version
	if ! IsGolangCiLintCorrectVersion "${GOLANGCI_LINT_VERSION}"; then
		installRequired="true"
	fi

	# Install the correct version if we need to
	if [[ "${installRequired}" == "true" ]]; then
		if ! DownloadGolangCiLint "${GOLANGCI_LINT_VERSION}"; then
			echo "failed to install golangci-lint"
			return 1
		fi
	else
		echo "golangci-lint already installed and correct version (${GOLANGCI_LINT_VERSION})"
	fi

	# Execute the requested command
	if ! RunGolangCiLint "$@"; then
		echo "golangci-lint failed"
		return 1
	fi

	# All passed!
	return 0
}

if Main "$@"; then
	exit 0
fi

exit 1
