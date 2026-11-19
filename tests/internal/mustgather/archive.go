package mustgather

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ArtifactDirEnvVar is set by Prow to the directory whose contents are
	// collected and published with the job's results.
	ArtifactDirEnvVar = "ARTIFACT_DIR"

	// ReportsDirEnvVar is this repository's own output directory, used when a
	// run is not a Prow job. test-runner.sh points it at ARTIFACT_DIR in CI.
	ReportsDirEnvVar = "ECO_REPORTS_DUMP_DIR"

	// DefaultArchiveDir is where archives go when neither environment
	// variable is set, so a bare `ginkgo` invocation still leaves its
	// diagnostics behind rather than discarding them with the temporary
	// directory. A single spec run by hand is the case most likely to set
	// neither, and the case where losing the output is most annoying.
	//
	// tools/watch/collection-watch.sh falls back to the same path so it
	// finds these without being told; keep the two in step.
	DefaultArchiveDir = "/tmp/rhwa-collection-artifacts"

	// archiveTimestampLayout keeps archive names sortable and free of
	// characters that need quoting.
	archiveTimestampLayout = "20060102-150405"

	// archiveNamePrefix and archiveNameSuffix bracket the operator acronyms
	// in an archive's name, giving rhwa-<acronyms>-must-gather-<timestamp>.
	archiveNamePrefix = "rhwa"
	archiveNameSuffix = "must-gather"

	// archiveNameFixedParts counts the prefix, suffix and timestamp, which
	// are present whatever the acronyms are.
	archiveNameFixedParts = 3

	archiveFilePermissions = 0o644
)

// ArchiveDestination returns the directory diagnostic archives are written
// to: ARTIFACT_DIR when the job sets one, otherwise ECO_REPORTS_DUMP_DIR,
// otherwise DefaultArchiveDir. It never returns "", so a collection is always
// kept somewhere predictable and the caller never has to handle "nowhere to
// put this".
func ArchiveDestination() string {
	for _, envVar := range []string{ArtifactDirEnvVar, ReportsDirEnvVar} {
		if dir := os.Getenv(envVar); dir != "" {
			return dir
		}
	}

	return DefaultArchiveDir
}

// ArchiveName builds the file name for a collection archive:
//
//	rhwa-<operator acronyms>-must-gather-<UTC timestamp>.tar.gz
//	rhwa-snr-must-gather-20261029-212658.tar.gz
//	rhwa-nmo-snr-must-gather-20261029-213914.tar.gz
//
// The acronyms name the operators whose custom resources the collection
// actually holds, so the file says what is in it without being opened. They
// are sorted, which keeps the name stable for the same set of operators
// rather than depending on the order the inventory happened to list them.
// The timestamp keeps repeated runs in one artifact directory from
// overwriting each other. With no acronyms the name is still well formed:
// rhwa-must-gather-<timestamp>.tar.gz.
func ArchiveName(acronyms []string) string {
	parts := make([]string, 0, len(acronyms)+archiveNameFixedParts)
	parts = append(parts, archiveNamePrefix)
	parts = append(parts, acronyms...)
	parts = append(parts, archiveNameSuffix, time.Now().UTC().Format(archiveTimestampLayout))

	return strings.Join(parts, "-") + ".tar.gz"
}

// Archive writes sourceDir into archivePath as a gzipped tar and returns the
// size of the archive in bytes. Entries are stored under a single top-level
// directory named after the archive itself, so `tar xzf foo.tar.gz` creates
// `foo/` -- never scattering files into the current directory, and never
// leaking the temporary directory the collection happened to be written to.
//
// A must-gather tree is mostly text and compresses by roughly an order of
// magnitude, which is why the archive is published instead of the raw tree.
func Archive(sourceDir, archivePath string) (int64, error) {
	archiveFile, err := os.OpenFile(archivePath,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, archiveFilePermissions)
	if err != nil {
		return 0, fmt.Errorf("creating archive %s: %w", archivePath, err)
	}

	writeErr := writeArchive(sourceDir, archiveRootName(archivePath), archiveFile)
	closeErr := archiveFile.Close()

	if writeErr != nil {
		return 0, writeErr
	}

	if closeErr != nil {
		return 0, fmt.Errorf("closing archive %s: %w", archivePath, closeErr)
	}

	info, statErr := os.Stat(archivePath)
	if statErr != nil {
		return 0, fmt.Errorf("sizing archive %s: %w", archivePath, statErr)
	}

	return info.Size(), nil
}

// archiveRootName returns the top-level directory entries are stored under:
// the archive's own file name without its extension, so the extracted
// directory and the archive match.
func archiveRootName(archivePath string) string {
	base := filepath.Base(archivePath)
	for _, extension := range []string{".tar.gz", ".tgz"} {
		if trimmed := strings.TrimSuffix(base, extension); trimmed != base {
			return trimmed
		}
	}

	return base
}

// writeArchive streams sourceDir through tar and gzip into out, storing every
// entry under root.
func writeArchive(sourceDir, root string, out io.Writer) error {
	gzipWriter := gzip.NewWriter(out)
	tarWriter := tar.NewWriter(gzipWriter)

	walkErr := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", path, err)
		}

		return addArchiveEntry(tarWriter, sourceDir, root, path, info)
	})
	if walkErr != nil {
		// Both writers are still closed so the partial archive is at least a
		// valid stream, but the error from the walk is what matters.
		_ = tarWriter.Close()
		_ = gzipWriter.Close()

		return walkErr
	}

	if err := tarWriter.Close(); err != nil {
		_ = gzipWriter.Close()

		return fmt.Errorf("closing tar stream: %w", err)
	}

	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("closing gzip stream: %w", err)
	}

	return nil
}

// addArchiveEntry writes one filesystem entry into the tar stream. Regular
// files, directories and symlinks are preserved; anything else (a socket or
// device left behind in a collected tree) is skipped rather than failing the
// archive.
func addArchiveEntry(tarWriter *tar.Writer, sourceDir, root, path string, info os.FileInfo) error {
	relPath, relErr := filepath.Rel(sourceDir, path)
	if relErr != nil {
		return fmt.Errorf("relative path for %s: %w", path, relErr)
	}

	linkTarget := ""
	if info.Mode()&os.ModeSymlink != 0 {
		var linkErr error
		if linkTarget, linkErr = os.Readlink(path); linkErr != nil {
			return fmt.Errorf("reading link %s: %w", path, linkErr)
		}
	}

	if !info.Mode().IsRegular() && !info.IsDir() && linkTarget == "" {
		return nil
	}

	header, headerErr := tar.FileInfoHeader(info, linkTarget)
	if headerErr != nil {
		return fmt.Errorf("tar header for %s: %w", path, headerErr)
	}

	header.Name = filepath.ToSlash(filepath.Join(root, relPath))
	if info.IsDir() {
		header.Name += "/"
	}

	if err := tarWriter.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	source, openErr := os.Open(path)
	if openErr != nil {
		return fmt.Errorf("opening %s: %w", path, openErr)
	}
	defer source.Close()

	if _, err := io.Copy(tarWriter, source); err != nil {
		return fmt.Errorf("copying %s into archive: %w", path, err)
	}

	return nil
}
