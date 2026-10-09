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

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	oplmV1alpha1 "github.com/rh-ecosystem-edge/eco-goinfra/pkg/schemes/olm/operators/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestRunDigestRefSkipsImageInfo(t *testing.T) {
	// `oc image info` fails; a digest-pinned ref must still log its digest
	// from the ref itself rather than falling back to a WARNING.
	fakeOC(t, `if [ "$1" = image ]; then exit 1; fi
exit 0
`)
	dest := t.TempDir()
	var logs strings.Builder
	image := "registry.redhat.io/workload-availability/must-gather@sha256:abc123"
	if err := Run(context.Background(), image, dest, Options{ImageInfoTimeout: testTimeout},
		func(format string, args ...interface{}) { fmt.Fprintf(&logs, format, args...) }); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(logs.String(), "WARNING") {
		t.Fatalf("digest ref should not warn: %s", logs.String())
	}

	if !strings.Contains(logs.String(), "digest sha256:abc123") {
		t.Fatalf("embedded digest not logged: %s", logs.String())
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

const (
	testEnvVar       = "MUST_GATHER_IMAGE"
	testNamespace    = "openshift-workload-availability"
	testDefaultImage = "quay.io/medik8s/must-gather:latest"
	testEnvImage     = "mirror.example.com/must-gather:mirrored"
	testCSVImage     = "registry.redhat.io/workload-availability/must-gather@sha256:abc"
)

// csvClient returns a fake *clients.Settings holding the given CSVs.
func csvClient(t *testing.T, csvs ...*oplmV1alpha1.ClusterServiceVersion) *clients.Settings {
	t.Helper()
	objects := make([]runtime.Object, 0, len(csvs))
	for _, csv := range csvs {
		objects = append(objects, csv)
	}

	return clients.GetTestClients(clients.TestClientParams{
		K8sMockObjects:  objects,
		SchemeAttachers: []clients.SchemeAttacher{oplmV1alpha1.AddToScheme},
	})
}

// csvWithRelatedImage builds a CSV carrying a single relatedImages entry.
func csvWithRelatedImage(name, relatedName, image string) *oplmV1alpha1.ClusterServiceVersion {
	return &oplmV1alpha1.ClusterServiceVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: oplmV1alpha1.ClusterServiceVersionSpec{
			RelatedImages: []oplmV1alpha1.RelatedImage{{Name: relatedName, Image: image}},
		},
	}
}

func TestDiscoverImage(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       string
		apiClient *clients.Settings
		want      string
	}{
		{
			name:      "env var wins over discovery and default",
			env:       testEnvImage,
			apiClient: csvClient(t, csvWithRelatedImage("nhc.v0.1", "must_gather", testCSVImage)),
			want:      testEnvImage,
		},
		{
			name:      "discovers must_gather from CSV relatedImages",
			apiClient: csvClient(t, csvWithRelatedImage("nhc.v0.1", "must_gather", testCSVImage)),
			want:      testCSVImage,
		},
		{
			name:      "accepts hyphenated must-gather name case-insensitively",
			apiClient: csvClient(t, csvWithRelatedImage("nhc.v0.1", "Must-Gather", testCSVImage)),
			want:      testCSVImage,
		},
		{
			name:      "falls back to default when no CSV carries must-gather",
			apiClient: csvClient(t, csvWithRelatedImage("far.v0.1", "fence-agents-remediation", "other")),
			want:      testDefaultImage,
		},
		{
			name:      "falls back to default with nil client",
			apiClient: nil,
			want:      testDefaultImage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(testEnvVar, test.env)
			got := DiscoverImage(test.apiClient, testNamespace, testEnvVar, testDefaultImage,
				func(string, ...interface{}) {})
			if got != test.want {
				t.Fatalf("DiscoverImage = %q, want %q", got, test.want)
			}
		})
	}
}
