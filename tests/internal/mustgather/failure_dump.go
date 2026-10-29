package mustgather

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	failureDiagnosticsDir = "failure-diagnostics"
	gatherNamespacePrefix = "openshift-must-gather-"
)

// DumpFailureDiagnostics collects best-effort evidence after a must-gather run
// fails: gather-namespace pods/events/describe/logs and optional node describes.
// Missing namespaces or pods are logged and skipped; this never returns an error
// to the caller. Files are written under destDir/failure-diagnostics/ and a
// short summary is emitted via logf.
func DumpFailureDiagnostics(
	ctx context.Context,
	destDir string,
	since time.Time,
	nodeNames []string,
	timeout time.Duration,
	logf func(format string, args ...interface{}),
) {
	if timeout <= 0 {
		timeout = DefaultCleanupTimeout
	}

	dumpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	diagDir := filepath.Join(destDir, failureDiagnosticsDir)
	if mkdirErr := os.MkdirAll(diagDir, 0o755); mkdirErr != nil {
		logf("WARNING: failure dump: could not create %s: %v\n", diagDir, mkdirErr)

		return
	}

	logf("failure dump: writing diagnostics under %s\n", diagDir)
	namespaces, listErr := listGatherNamespacesSince(dumpCtx, since)
	if listErr != nil {
		msg := fmt.Sprintf("failed to list gather namespaces: %v\n", listErr)
		writeDumpFile(diagDir, "namespaces.txt", []byte(msg), logf)
		logf("WARNING: failure dump: %s", msg)

		return
	}

	if len(namespaces) == 0 {
		msg := fmt.Sprintf("no gather namespace found with prefix %s since %s\n",
			gatherNamespacePrefix, since.UTC().Format(time.RFC3339))
		writeDumpFile(diagDir, "namespaces.txt", []byte(msg), logf)
		logf("failure dump: %s", msg)
	} else {
		writeDumpFile(diagDir, "namespaces.txt",
			[]byte(strings.Join(namespaces, "\n")+"\n"), logf)
		logf("failure dump: gather namespaces: %s\n", strings.Join(namespaces, ", "))

		for _, ns := range namespaces {
			dumpGatherNamespace(dumpCtx, diagDir, ns, logf)
		}
	}

	for _, nodeName := range nodeNames {
		if nodeName == "" {
			continue
		}

		dumpNode(dumpCtx, diagDir, nodeName, logf)
	}
}

func dumpGatherNamespace(
	ctx context.Context, diagDir, namespace string, logf func(format string, args ...interface{}),
) {
	safeNS := sanitizeFileName(namespace)
	podsOut, podsErr := runOC(ctx, "get", "pods", "-n", namespace, "-o", "wide")
	skipPodDetails := false

	switch {
	case podsErr != nil:
		msg := fmt.Sprintf("oc get pods -n %s failed: %v\n%s\n", namespace, podsErr, podsOut)
		writeDumpFile(diagDir, safeNS+"-pods.txt", []byte(msg), logf)
		logf("WARNING: failure dump: no pods listed in %s: %v\n", namespace, podsErr)
		skipPodDetails = true
	case strings.TrimSpace(podsOut) == "" || strings.Contains(podsOut, "No resources found"):
		msg := fmt.Sprintf("no pods in gather namespace %s\n%s\n", namespace, podsOut)
		writeDumpFile(diagDir, safeNS+"-pods.txt", []byte(msg), logf)
		logf("failure dump: %s", msg)
		skipPodDetails = true
	default:
		writeDumpFile(diagDir, safeNS+"-pods.txt", []byte(podsOut), logf)
		logf("failure dump: pods in %s:\n%s\n", namespace, strings.TrimSpace(podsOut))
	}

	eventsOut, eventsErr := runOC(ctx, "get", "events", "-n", namespace,
		"--sort-by=.lastTimestamp")
	if eventsErr != nil {
		msg := fmt.Sprintf("oc get events -n %s failed: %v\n%s\n", namespace, eventsErr, eventsOut)
		writeDumpFile(diagDir, safeNS+"-events.txt", []byte(msg), logf)
		logf("WARNING: failure dump: events for %s: %v\n", namespace, eventsErr)
	} else {
		writeDumpFile(diagDir, safeNS+"-events.txt", []byte(eventsOut), logf)
		logf("failure dump: wrote events for %s\n", namespace)
	}

	if skipPodDetails {
		logf("failure dump: skipping pod describe/logs for %s (no usable pod list)\n", namespace)

		return
	}

	dumpGatherPodDetails(ctx, diagDir, namespace, safeNS, podsOut, logf)
}

func dumpGatherPodDetails(
	ctx context.Context,
	diagDir, namespace, safeNS, podsOut string,
	logf func(format string, args ...interface{}),
) {
	podNames := podNamesFromGetWide(podsOut)
	if len(podNames) == 0 {
		logf("failure dump: skipping pod describe/logs for %s (no pod names)\n", namespace)

		return
	}

	for _, podName := range podNames {
		safePod := sanitizeFileName(podName)
		describeOut, describeErr := runOC(ctx, "describe", "pod", podName, "-n", namespace)
		if describeErr != nil {
			msg := fmt.Sprintf("oc describe pod %s -n %s failed: %v\n%s\n",
				podName, namespace, describeErr, describeOut)
			writeDumpFile(diagDir, safeNS+"-pod-"+safePod+"-describe.txt", []byte(msg), logf)
			logf("WARNING: failure dump: describe pod %s/%s: %v\n", namespace, podName, describeErr)
		} else {
			writeDumpFile(diagDir, safeNS+"-pod-"+safePod+"-describe.txt", []byte(describeOut), logf)
			logf("failure dump: wrote describe for pod %s/%s\n", namespace, podName)
		}

		logsOut, logsErr := runOC(ctx, "logs", podName, "-n", namespace,
			"--all-containers", "--tail=200")
		if logsErr != nil {
			msg := fmt.Sprintf("oc logs %s -n %s failed: %v\n%s\n",
				podName, namespace, logsErr, logsOut)
			writeDumpFile(diagDir, safeNS+"-pod-"+safePod+"-logs.txt", []byte(msg), logf)
			logf("WARNING: failure dump: logs for pod %s/%s: %v\n", namespace, podName, logsErr)
		} else {
			writeDumpFile(diagDir, safeNS+"-pod-"+safePod+"-logs.txt", []byte(logsOut), logf)
			logf("failure dump: wrote logs for pod %s/%s\n", namespace, podName)
		}
	}
}

func dumpNode(
	ctx context.Context, diagDir, nodeName string, logf func(format string, args ...interface{}),
) {
	safeNode := sanitizeFileName(nodeName)
	out, err := runOC(ctx, "describe", "node", nodeName)
	if err != nil {
		msg := fmt.Sprintf("oc describe node %s failed: %v\n%s\n", nodeName, err, out)
		writeDumpFile(diagDir, "node-"+safeNode+"-describe.txt", []byte(msg), logf)
		logf("WARNING: failure dump: describe node %s: %v\n", nodeName, err)

		return
	}

	writeDumpFile(diagDir, "node-"+safeNode+"-describe.txt", []byte(out), logf)
	logf("failure dump: wrote describe for node %s\n", nodeName)
}

func listGatherNamespacesSince(ctx context.Context, since time.Time) ([]string, error) {
	out, err := exec.CommandContext(ctx, "oc", "get", "ns",
		"-l", "openshift.io/run-level",
		"-o", "jsonpath={range .items[*]}{.metadata.name} {.metadata.creationTimestamp}{\"\\n\"}{end}",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("oc get ns: %w\n%s", err, string(out))
	}

	var names []string

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], gatherNamespacePrefix) {
			continue
		}

		if len(fields) >= namespaceTimestampFields {
			createdAt, parseErr := time.Parse(time.RFC3339, fields[1])
			if parseErr == nil && createdAt.Before(since) {
				continue
			}
		}

		names = append(names, fields[0])
	}

	return names, nil
}

func runOC(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "oc", args...).CombinedOutput()

	return string(out), err
}

func writeDumpFile(
	dir, name string, data []byte, logf func(format string, args ...interface{}),
) {
	path := filepath.Join(dir, name)
	if writeErr := os.WriteFile(path, data, commandLogPermissions); writeErr != nil {
		logf("WARNING: failure dump: failed to write %s: %v\n", path, writeErr)
	}
}

func sanitizeFileName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ":", "_")

	return replacer.Replace(name)
}

// podNamesFromGetWide extracts pod names from `oc get pods -o wide` table output.
func podNamesFromGetWide(out string) []string {
	var names []string

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "NAME" {
			continue
		}

		names = append(names, fields[0])
	}

	return names
}
