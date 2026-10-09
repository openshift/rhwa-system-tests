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

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/olm"
)

const (
	commandLogPermissions    = 0o644
	namespaceTimestampFields = 2
)

// relatedImageMustGatherNames are the names under which the version-matched
// downstream must-gather image is published in an operator CSV's
// .spec.relatedImages. OSBS derives the entry name from the RELATED_IMAGE_MUST_GATHER
// env var the NHC bundle injects, lowercasing the suffix to "must_gather"; the
// hyphenated form is accepted defensively in case the convention changes.
var relatedImageMustGatherNames = []string{"must_gather", "must-gather"}

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
	// A digest-pinned ref already carries its provenance, so log it directly and
	// skip the client-side `oc image info` lookup, which needs registry creds the
	// test pod may lack (e.g. registry.redhat.io on downstream builds).
	if _, digest, found := strings.Cut(image, "@"); found {
		logf("Using must-gather image %s (digest %s)\n", image, digest)
	} else if digest, digestErr := resolveImageDigest(ctx, image, options.ImageInfoTimeout); digestErr != nil {
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

// DiscoverImage resolves the must-gather image a spec should run, in order of
// preference:
//
//  1. the envVar override, when set (lets disconnected CI inject the mirrored ref);
//  2. the version-matched downstream build, scraped from an installed operator
//     CSV's .spec.relatedImages (the runtime analog of rendering the FBC catalog);
//  3. defaultImage, the community image, as a last resort.
//
// The chosen source is logged so a run's image provenance is visible in the test
// output. A nil apiClient or a failed cluster lookup is non-fatal: discovery is
// skipped and resolution falls through to defaultImage.
func DiscoverImage(
	apiClient *clients.Settings,
	namespace, envVar, defaultImage string,
	logf func(format string, args ...interface{}),
) string {
	if envImg := os.Getenv(envVar); envImg != "" {
		logf("must-gather image resolved from %s env var: %s\n", envVar, envImg)

		return envImg
	}

	if img := imageFromCSVRelatedImages(apiClient, namespace, logf); img != "" {
		logf("must-gather image discovered from CSV relatedImages: %s\n", img)

		return img
	}

	logf("must-gather image using default: %s\n", defaultImage)

	return defaultImage
}

// imageFromCSVRelatedImages scans every CSV installed in namespace for a
// .spec.relatedImages entry naming the must-gather image, returning the first
// match (empty string when none is found or the lookup fails). The must-gather
// image is pinned only in the NHC CSV, but the scan is operator-agnostic so it
// keeps working whichever operator ends up carrying it.
func imageFromCSVRelatedImages(
	apiClient *clients.Settings, namespace string, logf func(format string, args ...interface{}),
) string {
	if apiClient == nil {
		return ""
	}

	csvs, err := olm.ListClusterServiceVersion(apiClient, namespace)
	if err != nil {
		logf("WARNING: could not list CSVs in %s for must-gather discovery: %v\n", namespace, err)

		return ""
	}

	for _, csv := range csvs {
		for _, related := range csv.Object.Spec.RelatedImages {
			for _, name := range relatedImageMustGatherNames {
				if strings.EqualFold(related.Name, name) && related.Image != "" {
					return related.Image
				}
			}
		}
	}

	return ""
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
	out, err := exec.CommandContext(cleanupCtx, "oc", "get", "ns",
		"-l", "openshift.io/run-level",
		"-o", "jsonpath={range .items[*]}{.metadata.name} {.metadata.creationTimestamp}{\"\\n\"}{end}",
	).CombinedOutput()
	if err != nil {
		logf("Warning: failed to list namespaces for must-gather cleanup: %v\n", err)

		return
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "openshift-must-gather-") {
			continue
		}

		namespaceName := fields[0]
		if len(fields) >= namespaceTimestampFields {
			createdAt, parseErr := time.Parse(time.RFC3339, fields[1])
			if parseErr == nil && createdAt.Before(testStartTime) {
				continue
			}
		}

		logf("Cleaning up leftover must-gather namespace: %s\n", namespaceName)
		cleanupOut, cleanupErr := exec.CommandContext(cleanupCtx, "oc", "delete", "ns", namespaceName,
			"--ignore-not-found", "--wait=false").CombinedOutput()
		if cleanupErr != nil {
			logf("Warning: failed to delete namespace %s: %v\n%s\n",
				namespaceName, cleanupErr, string(cleanupOut))
		}
	}
}
