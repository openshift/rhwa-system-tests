//nolint:lll // Long fixture commands are kept intact so their exact shell inputs remain reviewable.
package nhcutils

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeScriptFixture(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerPreservesReportsAndOriginalFailure(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	ginkgo := filepath.Join(dir, "ginkgo")
	writeScriptFixture(t, ginkgo, `#!/usr/bin/env bash
mkdir -p "$ECO_REPORTS_DUMP_DIR"
printf '<report/>\n' > "$ECO_REPORTS_DUMP_DIR/nhc_testrun.xml"
printf 'Ran 1 of 24 Specs\noriginal test failure\n'
exit 7
`)
	t.Setenv("GINKGO", ginkgo)
	t.Setenv("ECO_TEST_FEATURES", "nhc-operator")
	t.Setenv("ECO_TEST_LABELS", "tier:upgrade-operator")
	t.Setenv("SHARED_DIR", filepath.Join(dir, "missing-copy-destination"))

	for _, ci := range []bool{false, true} {
		reports := filepath.Join(dir, "local")
		t.Setenv("ECO_REPORTS_DUMP_DIR", reports)
		t.Setenv("ARTIFACT_DIR", "")

		if ci {
			reports = filepath.Join(dir, "artifacts")
			t.Setenv("ARTIFACT_DIR", reports)
		}

		command := exec.Command("bash", "scripts/test-runner.sh")
		command.Dir = root
		output, err := command.CombinedOutput()
		exitError := &exec.ExitError{}
		ok := errors.As(err, &exitError)
		if !ok || exitError.ExitCode() != 7 || !strings.Contains(string(output), "original test failure") {
			t.Fatalf("lost original failure: %v %s", err, output)
		}

		if _, err := os.Stat(filepath.Join(reports, "nhc_testrun.xml")); err != nil {
			t.Fatal("report missing", err)
		}
	}
}

func TestPreparationUsesExactMakeInputs(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	// The helper rejects /tmp build paths. CI checks out system-tests under
	// /tmp, so place the fixture in its writable artifact directory.
	fixtureParent := "."
	if artifactDir := os.Getenv("ARTIFACT_DIR"); artifactDir != "" {
		fixtureParent = artifactDir
	}
	//nolint:usetesting // The fixture must live outside /tmp when CI runs this test.
	dir, err := os.MkdirTemp(fixtureParent, "prepare-fixture-")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	writeScriptFixture(t, filepath.Join(dir, "git"), `#!/usr/bin/env bash
set -eu
printf 'git %s\n' "$*" >> "$COMMAND_LOG"
if [[ $1 == clone ]]; then mkdir -p "$4/.git";
elif [[ $3 == rev-parse ]]; then printf '%s\n' "$NHC_EXPECTED_SOURCE_COMMIT";
fi
`)
	writeScriptFixture(t, filepath.Join(dir, "podman"), "#!/usr/bin/env bash\nprintf 'podman %s\\n' \"$*\" >> \"$COMMAND_LOG\"\n")
	writeScriptFixture(t, filepath.Join(dir, "make"), "#!/usr/bin/env bash\nprintf 'make %s\\n' \"$*\" >> \"$COMMAND_LOG\"\nmkdir -p \"$NHC_BUILD_DIR/bundle\"\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("COMMAND_LOG", filepath.Join(dir, "commands"))
	t.Setenv("NHC_SOURCE_REPOSITORY", filepath.Join(dir, "source"))
	t.Setenv("NHC_BUILD_DIR", filepath.Join(dir, "build"))
	t.Setenv("NHC_BUILD_REPORT_DIR", filepath.Join(dir, "reports"))
	t.Setenv("NHC_EXPECTED_SOURCE_COMMIT", strings.Repeat("a", 40))
	t.Setenv("VERSION", "5.8.0")
	t.Setenv("OPERATOR_BUILD_IMAGE", "registry.test/operator:build")
	t.Setenv("BUNDLE_BUILD_IMAGE", "registry.test/bundle:build")
	t.Setenv("CONSOLE_PLUGIN_IMAGE", "registry.test/console@sha256:"+strings.Repeat("1", 64))
	t.Setenv("MUST_GATHER_IMAGE", "registry.test/gather@sha256:"+strings.Repeat("2", 64))
	t.Setenv("NHC_OPERATOR_IMAGE", "registry.test/operator@sha256:"+strings.Repeat("3", 64))

	for _, stage := range []string{"operator", "bundle"} {
		output, err := exec.Command("bash", filepath.Join(root, "scripts/nhc-upgrade-prepare.sh"), stage).CombinedOutput()
		if err != nil {
			t.Fatalf("stage %s: %v\n%s", stage, err, output)
		}
	}

	commands, err := os.ReadFile(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}

	for _, required := range []string{"clone --no-hardlinks", "org.opencontainers.image.revision=", "bundle-build-ocp VERSION=5.8.0", "IMG=" + os.Getenv("NHC_OPERATOR_IMAGE"), "BUNDLE_IMG=registry.test/bundle:build", "CONSOLE_PLUGIN_IMAGE=", "MUST_GATHER_IMAGE=", "PREVIOUS_VERSION=0.12.0 SKIP_RANGE_LOWER=0.1.0"} {
		if !strings.Contains(string(commands), required) {
			t.Fatalf("missing %q in %s", required, commands)
		}
	}

	if strings.Contains(string(commands), "podman push") {
		t.Fatal("helper unexpectedly publishes images")
	}
}
