package mustgather

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readArchive returns the entry names in a gzipped tar, and the contents of
// every regular file, keyed by entry name.
func readArchive(t *testing.T, archivePath string) ([]string, map[string]string) {
	t.Helper()
	archiveFile, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archiveFile.Close()

	gzipReader, err := gzip.NewReader(archiveFile)
	if err != nil {
		t.Fatalf("archive is not valid gzip: %v", err)
	}
	defer gzipReader.Close()

	var names []string

	contents := make(map[string]string)
	tarReader := tar.NewReader(gzipReader)

	for {
		header, readErr := tarReader.Next()
		if readErr == io.EOF {
			break
		}

		if readErr != nil {
			t.Fatalf("archive is not a valid tar: %v", readErr)
		}

		names = append(names, header.Name)

		if header.Typeflag == tar.TypeReg {
			body, copyErr := io.ReadAll(tarReader)
			if copyErr != nil {
				t.Fatal(copyErr)
			}

			contents[header.Name] = string(body)
		}
	}

	sort.Strings(names)

	return names, contents
}

func TestArchive(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "must-gather-output")
	writeTree(t, sourceDir, "cluster-scoped-resources/core/nodes/worker-0.yaml", "version")

	if err := os.WriteFile(filepath.Join(sourceDir, "version"), []byte("medik8s/must-gather\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "collection.tar.gz")
	size, err := Archive(sourceDir, archivePath)
	if err != nil {
		t.Fatalf("Archive returned %v", err)
	}

	if size <= 0 {
		t.Errorf("Archive reported size %d, want a positive size", size)
	}

	names, contents := readArchive(t, archivePath)

	// Every entry must sit under one top-level directory, so extracting the
	// archive cannot scatter files into the current directory.
	// The top-level directory is named after the archive, not after the
	// temporary directory the collection was written to, so extracting
	// collection.tar.gz yields collection/.
	for _, name := range names {
		if !strings.HasPrefix(name, "collection/") {
			t.Errorf("entry %q is not under the top-level directory", name)
		}
	}

	for _, want := range []string{
		"collection/cluster-scoped-resources/core/nodes/worker-0.yaml",
		"collection/version",
	} {
		if _, found := contents[want]; !found {
			t.Errorf("archive is missing %q; it holds %v", want, names)
		}
	}

	if got := contents["collection/version"]; got != "medik8s/must-gather\n" {
		t.Errorf("version content = %q, want the written bytes", got)
	}
}

// TestArchiveSymlink covers a link in a collected tree: it must be recorded as
// a link rather than dereferenced or treated as an error.
func TestArchiveSymlink(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "tree")
	writeTree(t, sourceDir, "real.yaml")

	if err := os.Symlink("real.yaml", filepath.Join(sourceDir, "link.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "collection.tar.gz")
	if _, err := Archive(sourceDir, archivePath); err != nil {
		t.Fatalf("Archive returned %v", err)
	}

	names, _ := readArchive(t, archivePath)
	if !slicesContain(names, "collection/link.yaml") {
		t.Errorf("archive is missing the symlink entry; it holds %v", names)
	}
}

func TestArchiveMissingSource(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "collection.tar.gz")
	if _, err := Archive(filepath.Join(t.TempDir(), "absent"), archivePath); err == nil {
		t.Error("Archive on a missing source directory returned no error")
	}
}

func TestArchiveUnwritableDestination(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "tree")
	writeTree(t, sourceDir, "a.yaml")

	if _, err := Archive(sourceDir, filepath.Join(t.TempDir(), "no-such-dir", "x.tar.gz")); err == nil {
		t.Error("Archive to an unwritable path returned no error")
	}
}

func TestArchiveDestination(t *testing.T) {
	t.Run("prefers ARTIFACT_DIR", func(t *testing.T) {
		t.Setenv(ArtifactDirEnvVar, "/artifacts")
		t.Setenv(ReportsDirEnvVar, "/reports")

		if got := ArchiveDestination(); got != "/artifacts" {
			t.Errorf("ArchiveDestination = %q, want /artifacts", got)
		}
	})

	t.Run("falls back to the reports directory", func(t *testing.T) {
		t.Setenv(ArtifactDirEnvVar, "")
		t.Setenv(ReportsDirEnvVar, "/reports")

		if got := ArchiveDestination(); got != "/reports" {
			t.Errorf("ArchiveDestination = %q, want /reports", got)
		}
	})

	// Never "": a run that configures nothing still has to keep its
	// collection somewhere predictable, which is the whole point of the
	// default.
	t.Run("defaults when neither is set", func(t *testing.T) {
		t.Setenv(ArtifactDirEnvVar, "")
		t.Setenv(ReportsDirEnvVar, "")

		if got := ArchiveDestination(); got != DefaultArchiveDir {
			t.Errorf("ArchiveDestination = %q, want %q", got, DefaultArchiveDir)
		}
	})
}

func TestArchiveRootName(t *testing.T) {
	for _, testCase := range []struct{ path, want string }{
		{"/artifacts/rhwa-collection-20261029-212658.tar.gz", "rhwa-collection-20261029-212658"},
		{"/artifacts/collection.tgz", "collection"},
		{"/artifacts/collection", "collection"},
	} {
		if got := archiveRootName(testCase.path); got != testCase.want {
			t.Errorf("archiveRootName(%q) = %q, want %q", testCase.path, got, testCase.want)
		}
	}
}

// TestDefaultArchiveDirMatchesWatchScript guards the one value duplicated
// between Go and shell. tools/watch/collection-watch.sh cannot import this
// constant, so it hardcodes the same path; if the two drift, the watch script
// silently reports an empty directory while the specs write elsewhere, and
// nothing else would catch it.
func TestDefaultArchiveDirMatchesWatchScript(t *testing.T) {
	const scriptPath = "../../../tools/watch/collection-watch.sh"

	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Skipf("watch script unavailable: %v", err)
	}

	if !strings.Contains(string(script), DefaultArchiveDir) {
		t.Errorf("%s does not contain %q: the Go default and the script's fallback have drifted",
			scriptPath, DefaultArchiveDir)
	}
}

func TestArchiveName(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		acronyms []string
		want     string
	}{
		{name: "single operator", acronyms: []string{"snr"}, want: "rhwa-snr-must-gather-"},
		{name: "several operators", acronyms: []string{"sbr", "snr"}, want: "rhwa-sbr-snr-must-gather-"},
		{
			name:     "every operator",
			acronyms: []string{"far", "mdr", "nhc", "nmo", "sbr", "snr"},
			want:     "rhwa-far-mdr-nhc-nmo-sbr-snr-must-gather-",
		},
		// No live instances still has to produce a well-formed name.
		{name: "none", acronyms: nil, want: "rhwa-must-gather-"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := ArchiveName(testCase.acronyms)
			if !strings.HasPrefix(got, testCase.want) {
				t.Errorf("ArchiveName(%v) = %q, want prefix %q", testCase.acronyms, got, testCase.want)
			}

			if !strings.HasSuffix(got, ".tar.gz") {
				t.Errorf("ArchiveName(%v) = %q, want a .tar.gz suffix", testCase.acronyms, got)
			}

			if strings.Contains(got, "--") {
				t.Errorf("ArchiveName(%v) = %q, want no doubled separator", testCase.acronyms, got)
			}
		})
	}
}

// TestArchiveNameRoundTrip pins that the archive's name is also the directory
// it extracts to, which archiveRootName derives by stripping the extension.
func TestArchiveNameRoundTrip(t *testing.T) {
	name := ArchiveName([]string{"snr", "sbr"})
	root := archiveRootName("/artifacts/" + name)
	if root+".tar.gz" != name {
		t.Errorf("archiveRootName(%q) = %q, which does not round-trip", name, root)
	}
}

func slicesContain(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}

	return false
}
