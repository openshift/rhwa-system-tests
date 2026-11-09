package mustgather

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testTimeout               = time.Second
	testDirectoryPermissions  = 0o755
	testExecutablePermissions = 0o755
	expectedMissingItems      = 3
)

func fakeOC(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oc"), []byte("#!/bin/sh\n"+script), testExecutablePermissions); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCreateDestDirCreatesMissingParent(t *testing.T) {
	for _, useArtifactDir := range []bool{true, false} {
		t.Run(fmt.Sprintf("artifactDir=%t", useArtifactDir), func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "missing", "artifacts")
			fallback := base
			t.Setenv("ARTIFACT_DIR", "")
			if useArtifactDir {
				t.Setenv("ARTIFACT_DIR", base)
				fallback = t.TempDir()
			}
			first, err := CreateDestDir("sbr-must-gather-", fallback)
			if err != nil {
				t.Fatal(err)
			}
			second, err := CreateDestDir("sbr-must-gather-", fallback)
			if err != nil {
				t.Fatal(err)
			}
			if first == second || filepath.Dir(first) != base || filepath.Dir(second) != base {
				t.Fatalf("expected unique directories under %s; got %s and %s", base, first, second)
			}
			info, err := os.Stat(first)
			if err != nil || !info.IsDir() {
				t.Fatalf("output directory was not created: %v", err)
			}
		})
	}
}

func TestCreateDestDirRejectsFileParent(t *testing.T) {
	base := filepath.Join(t.TempDir(), "artifact-file")
	if err := os.WriteFile(base, nil, commandLogPermissions); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTIFACT_DIR", base)
	if dest, err := CreateDestDir("sbr-must-gather-", t.TempDir()); err == nil || dest != "" {
		t.Fatalf("expected invalid artifact parent to fail, got destination %q and error %v", dest, err)
	}
}

func TestCreateDestDirUsesDefaultTempDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("ARTIFACT_DIR", "")
	dest, err := CreateDestDir("sbr-must-gather-", "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dest) != base {
		t.Fatalf("expected default temporary directory %s, got %s", base, dest)
	}
}

func TestRun(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("commandFailure=%t", fail), func(t *testing.T) {
			fakeOC(t, `if [ "$1" = image ]; then
  printf '{"digest":"sha256:test"}'
  exit 0
fi
printf '%s\n' "$*" "HOME=$HOME"
if [ "$FAIL_COLLECTION" = true ]; then exit 1; fi
`)
			t.Setenv("HOME", "")
			t.Setenv("FAIL_COLLECTION", fmt.Sprint(fail))
			dest := t.TempDir()
			var logs strings.Builder
			logf := func(format string, args ...interface{}) { fmt.Fprintf(&logs, format, args...) }
			err := Run(context.Background(), "mirror/image@sha256:test", dest, Options{
				ImageInfoTimeout: testTimeout,
				OCTimeout:        testTimeout,
				SaveCommandLog:   true,
				HomeFallback:     true,
			}, logf)
			if (err != nil) != fail {
				t.Fatalf("Run error = %v, failure expected = %t", err, fail)
			}

			output, readErr := os.ReadFile(filepath.Join(dest, "oc-adm-must-gather.log"))
			if readErr != nil {
				t.Fatal(readErr)
			}

			for _, want := range []string{
				"--image=mirror/image@sha256:test", "--dest-dir=" + dest,
				"--timeout=1s", "HOME=/tmp",
			} {
				if !strings.Contains(string(output), want) {
					t.Errorf("output %q missing %q", output, want)
				}
			}

			if !strings.Contains(logs.String(), "digest sha256:test") {
				t.Errorf("digest not logged: %s", logs.String())
			}
		})
	}
}

func TestRunFAROptionsAndDigestFailure(t *testing.T) {
	fakeOC(t, `if [ "$1" = image ]; then exit 1; fi
printf '%s\n' "$*" "HOME=$HOME" > "$COMMAND_RECORD"
`)
	dest := t.TempDir()
	record := filepath.Join(dest, "command.txt")
	t.Setenv("COMMAND_RECORD", record)
	t.Setenv("HOME", "")
	var logs strings.Builder
	err := Run(context.Background(), "image:tag", dest, Options{
		CommandTimeout:   testTimeout,
		ImageInfoTimeout: testTimeout,
	}, func(format string, args ...interface{}) { fmt.Fprintf(&logs, format, args...) })
	if err != nil {
		t.Fatal(err)
	}

	output, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(output), "--timeout=") || strings.Contains(string(output), "HOME=/tmp") {
		t.Fatalf("SBR options unexpectedly applied: %s", output)
	}

	if _, err := os.Stat(filepath.Join(dest, "oc-adm-must-gather.log")); !os.IsNotExist(err) {
		t.Fatalf("unexpected command log: %v", err)
	}

	if !strings.Contains(logs.String(), "WARNING") {
		t.Fatalf("digest failure not logged: %s", logs.String())
	}
}

func TestRunCanceledContext(t *testing.T) {
	fakeOC(t, "exit 0\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := t.TempDir()
	err := Run(ctx, "image:tag", dest, Options{
		ImageInfoTimeout: testTimeout,
		SaveCommandLog:   true,
	}, func(string, ...interface{}) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "oc-adm-must-gather.log")); err != nil {
		t.Fatalf("command log missing after cancellation: %v", err)
	}
}

func TestValidateAndCollectPaths(t *testing.T) {
	root := t.TempDir()
	resourceDir := filepath.Join(root, "nodes")
	if err := os.Mkdir(resourceDir, testDirectoryPermissions); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(resourceDir, "worker.yaml"), nil, commandLogPermissions); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(root, "directory.yaml"), testDirectoryPermissions); err != nil {
		t.Fatal(err)
	}

	missing := ValidateMustGatherContents(root, []MustGatherExpectation{
		{Description: "node", PathContains: "nodes", NameGlob: "*.yaml", MinCount: 1},
		{Description: "case-sensitive", PathContains: "NODES", NameGlob: "*.yaml", MinCount: 1},
		{Description: "not a file", NameGlob: "directory.yaml", MinCount: 1},
		{Description: "invalid glob", NameGlob: "[", MinCount: 1},
	})
	if len(missing) != expectedMissingItems {
		t.Fatalf("missing = %v", missing)
	}

	paths, err := CollectRelativePaths(root)
	if err != nil {
		t.Fatal(err)
	}

	if !HasMatchingFile(paths, "NODES/WORKER.YAML") || !HasMatchingFile(paths, "directory.yaml") {
		t.Fatalf("substring/path semantics changed: %v", paths)
	}

	if HasMatchingFile(paths, "absent") {
		t.Fatal("matched absent resource")
	}

	absent := filepath.Join(root, "absent")
	if _, err := CollectRelativePaths(absent); err == nil {
		t.Fatal("missing root did not return an error")
	}

	if missing := ValidateMustGatherContents(absent, nil); len(missing) == 0 {
		t.Fatal("missing root passed validation")
	}
}

func TestCleanupNamespaces(t *testing.T) {
	fakeOC(t, `if [ "$1" = get ]; then
  printf '%s\n' 'openshift-must-gather-old 2026-01-01T00:00:00Z' \
    'shared 2026-10-01T00:00:00Z' 'openshift-must-gather-new 2026-10-01T00:00:00Z'
else
  printf '%s\n' "$*" >> "$COMMAND_RECORD"
fi
`)
	record := filepath.Join(t.TempDir(), "deletions.txt")
	t.Setenv("COMMAND_RECORD", record)
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	CleanupNamespaces(context.Background(), start, testTimeout, func(string, ...interface{}) {})
	output, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	if string(output) != "delete ns openshift-must-gather-new --ignore-not-found --wait=false\n" {
		t.Fatalf("wrong cleanup targets: %s", output)
	}
}
