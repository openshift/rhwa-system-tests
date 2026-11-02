// Package mustgather provides operator-independent diagnostic collection and validation.
package mustgather

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	commandLogPermissions    = 0o644
	namespaceTimestampFields = 2
)

// Shared must-gather configuration, centralized here so every operator suite
// references one definition instead of copying these values into each
// operator's params package.
const (
	// ImageEnvVar overrides the must-gather image (e.g. a mirrored ref on
	// disconnected clusters). It takes precedence over DefaultImage.
	ImageEnvVar = "MUST_GATHER_IMAGE"

	// DefaultImage is the upstream medik8s must-gather image used when
	// ImageEnvVar is unset. The :latest tag is intentional so the test
	// exercises the build a user would actually pull; the resolved image
	// digest is logged on every run (see Run) so a failure is reproducible
	// against the exact build, and ImageEnvVar overrides it when a specific
	// ref is needed.
	DefaultImage = "quay.io/medik8s/must-gather:latest"

	// DefaultOCTimeout is the --timeout flag passed to oc adm must-gather so
	// it cleans up gracefully.
	DefaultOCTimeout = 14 * time.Minute

	// DefaultContextTimeout is the outer Go context timeout, strictly greater
	// than DefaultOCTimeout.
	DefaultContextTimeout = 16 * time.Minute

	// DefaultImageInfoTimeout bounds the best-effort `oc image info` digest lookup.
	DefaultImageInfoTimeout = 1 * time.Minute

	// DefaultCleanupTimeout is the deadline for best-effort cleanup of leftover
	// must-gather namespaces.
	DefaultCleanupTimeout = 2 * time.Minute
)

// Options preserves each suite's command execution and artifact requirements.
type Options struct {
	// CommandTimeout bounds only the collection subprocess, after digest lookup.
	// Leave it zero when the caller already bounds the entire operation.
	CommandTimeout   time.Duration
	ImageInfoTimeout time.Duration
	OCTimeout        time.Duration
	SaveCommandLog   bool
	HomeFallback     bool
}

// MustGatherExpectation describes a required artifact in the must-gather output.
// PathContains is matched against each file's relative path (substring match).
// NameGlob is matched against each file's basename using filepath.Match.
// Both conditions must be true for a file to count as a match.
// Set PathContains to "" to match any path (basename-only matching).
type MustGatherExpectation struct {
	Description  string
	PathContains string
	NameGlob     string
	MinCount     int
}

// Run collects diagnostics using the caller-selected image and logs its digest
// best-effort. ImageInfoTimeout bounds the lookup independently; the caller's
// context always bounds both lookup and collection. No assertions are made here.
func Run(
	ctx context.Context, image, destDir string, options Options, logf func(format string, args ...interface{}),
) error {
	if digest, digestErr := resolveImageDigest(ctx, image, options.ImageInfoTimeout); digestErr != nil {
		logf("WARNING: could not resolve must-gather image digest for %q: %v\n", image, digestErr)
	} else {
		logf("Using must-gather image %s (digest %s)\n", image, digest)
	}

	childCtx := ctx
	if options.CommandTimeout > 0 {
		var cancel context.CancelFunc
		childCtx, cancel = context.WithTimeout(ctx, options.CommandTimeout)
		defer cancel()
	}

	args := []string{
		"adm", "must-gather",
		"--image=" + image,
		"--dest-dir=" + destDir,
	}
	if options.OCTimeout > 0 {
		args = append(args, fmt.Sprintf("--timeout=%ds", int(options.OCTimeout.Seconds())))
	}

	cmd := exec.CommandContext(childCtx, "oc", args...)
	if options.HomeFallback && os.Getenv("HOME") == "" {
		cmd.Env = append(os.Environ(), "HOME=/tmp")
	}

	output, err := cmd.CombinedOutput()
	if options.SaveCommandLog {
		logFile := filepath.Join(destDir, "oc-adm-must-gather.log")
		if writeErr := os.WriteFile(logFile, output, commandLogPermissions); writeErr != nil {
			logf("Warning: failed to write must-gather log to %s: %v\n", logFile, writeErr)
		}

		logf("must-gather output saved to %s\n", logFile)
	}

	if childCtx.Err() != nil {
		return fmt.Errorf("must-gather context ended: %w\nOutput: %s", childCtx.Err(), string(output))
	}

	if err != nil {
		return fmt.Errorf("must-gather failed: %w\nOutput: %s", err, string(output))
	}

	return nil
}

// ResolveImage returns the must-gather image to use: the value of the
// envVar when set, otherwise defaultImage. The chosen source is logged so a
// run's image provenance is visible in the test output.
func ResolveImage(envVar, defaultImage string, logf func(format string, args ...interface{})) string {
	if envImg := os.Getenv(envVar); envImg != "" {
		logf("must-gather image resolved from %s env var: %s\n", envVar, envImg)

		return envImg
	}

	logf("must-gather image using default: %s\n", defaultImage)

	return defaultImage
}

// CreateDestDir creates a unique output directory for must-gather artifacts.
// It roots the directory at ARTIFACT_DIR when set (so CI collects the output),
// falling back to fallbackBase otherwise. prefix names the per-operator temp
// directory. It returns the created directory and any error from MkdirTemp.
func CreateDestDir(prefix, fallbackBase string) (string, error) {
	base := os.Getenv("ARTIFACT_DIR")
	if base == "" {
		base = fallbackBase
	}

	return os.MkdirTemp(base, prefix)
}

// resolveImageDigest returns the manifest digest a mutable image reference
// currently resolves to, so a run using a floating tag (e.g. :latest) can be
// reproduced against the exact build that was pulled. Best-effort: callers log
// the error and continue rather than failing the test.
func resolveImageDigest(ctx context.Context, image string, timeout time.Duration) (string, error) {
	infoCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(infoCtx, "oc", "image", "info", image,
		"--filter-by-os=linux/amd64", "-o", "json").Output()
	if err != nil {
		return "", fmt.Errorf("oc image info %s: %w", image, err)
	}

	var info struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parsing oc image info output: %w", err)
	}

	if info.Digest == "" {
		return "", fmt.Errorf("oc image info returned no digest for %s", image)
	}

	return info.Digest, nil
}

// ValidateMustGatherContents checks that the must-gather output directory
// contains all expected artifacts. Returns a list of missing expectations;
// an empty list means all checks passed.
func ValidateMustGatherContents(baseDir string, expectations []MustGatherExpectation) []string {
	allFiles, walkErr := collectFiles(baseDir)
	if walkErr != nil {
		return []string{fmt.Sprintf("failed to walk must-gather directory %s: %v", baseDir, walkErr)}
	}

	var missing []string

	for _, exp := range expectations {
		count := 0

		for _, f := range allFiles {
			if matchesExpectation(f.relPath, f.name, exp) {
				count++
			}
		}

		if count < exp.MinCount {
			missing = append(missing, fmt.Sprintf(
				"%s: expected at least %d match(es) (pathContains=%q, nameGlob=%q), found %d",
				exp.Description, exp.MinCount, exp.PathContains, exp.NameGlob, count))
		}
	}

	return missing
}

type fileEntry struct {
	relPath string
	name    string
}

func collectFiles(baseDir string) ([]fileEntry, error) {
	var files []fileEntry

	err := filepath.Walk(baseDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk error at %s: %w", path, walkErr)
		}

		if info.IsDir() {
			return nil
		}

		relPath, relErr := filepath.Rel(baseDir, path)
		if relErr != nil {
			return fmt.Errorf("failed to compute relative path for %s: %w", path, relErr)
		}

		files = append(files, fileEntry{
			relPath: relPath,
			name:    info.Name(),
		})

		return nil
	})

	return files, err
}

func matchesExpectation(relPath, name string, exp MustGatherExpectation) bool {
	if exp.PathContains != "" && !strings.Contains(relPath, exp.PathContains) {
		return false
	}

	if exp.NameGlob != "" {
		matched, err := filepath.Match(exp.NameGlob, name)
		if err != nil || !matched {
			return false
		}
	}

	return true
}

// CollectRelativePaths lists files and directories, including the root entry,
// for suites that validate collected resource directories as well as files.
func CollectRelativePaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		paths = append(paths, filepath.ToSlash(rel))

		return nil
	})

	return paths, err
}

// HasMatchingFile preserves the case-insensitive substring checks used by SBR.
// Unlike ValidateMustGatherContents, it also accepts directory paths.
func HasMatchingFile(files []string, pattern string) bool {
	lowerPattern := strings.ToLower(pattern)
	for _, file := range files {
		if strings.Contains(strings.ToLower(file), lowerPattern) {
			return true
		}
	}

	return false
}

// CleanupNamespaces requests asynchronous deletion of leftover must-gather
// namespaces created during this run. Errors are logged without failing a spec.
func CleanupNamespaces(
	ctx context.Context, testStartTime time.Time, timeout time.Duration, logf func(format string, args ...interface{}),
) {
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, timeout)
	defer cleanupCancel()

	namespaces, err := listGatherNamespacesSince(cleanupCtx, testStartTime)
	if err != nil {
		logf("Warning: failed to list namespaces for must-gather cleanup: %v\n", err)

		return
	}

	for _, namespaceName := range namespaces {
		logf("Cleaning up leftover must-gather namespace: %s\n", namespaceName)
		cleanupOut, cleanupErr := exec.CommandContext(cleanupCtx, "oc", "delete", "ns", namespaceName,
			"--ignore-not-found", "--wait=false").CombinedOutput()
		if cleanupErr != nil {
			logf("Warning: failed to delete namespace %s: %v\n%s\n",
				namespaceName, cleanupErr, string(cleanupOut))
		}
	}
}
