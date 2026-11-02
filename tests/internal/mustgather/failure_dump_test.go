package mustgather

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPodNamesFromGetWide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "table with header",
			out: "NAME   READY   STATUS\n" +
				"gather-abc   1/1   Running\n" +
				"gather-def   0/1   Pending\n",
			want: []string{"gather-abc", "gather-def"},
		},
		{
			name: "empty",
			out:  "",
			want: nil,
		},
		{
			name: "no resources found message",
			out:  "No resources found in openshift-must-gather-xyz namespace.\n",
			want: []string{"No"},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := podNamesFromGetWide(testCase.out)
			if len(got) != len(testCase.want) {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}

			for idx := range testCase.want {
				if got[idx] != testCase.want[idx] {
					t.Fatalf("got %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

func TestDumpFailureDiagnosticsNoNamespaces(t *testing.T) {
	fakeOC(t, `if [ "$1" = get ] && [ "$2" = ns ]; then
  printf '%s\n' 'other-ns 2026-10-01T00:00:00Z'
  exit 0
fi
exit 1
`)
	dest := t.TempDir()
	since := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	var logs strings.Builder
	DumpFailureDiagnostics(context.Background(), dest, since, nil, testTimeout,
		func(format string, args ...interface{}) { fmt.Fprintf(&logs, format, args...) })

	namespacesPath := filepath.Join(dest, failureDiagnosticsDir, "namespaces.txt")
	content, err := os.ReadFile(namespacesPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(content), "no gather namespace found") {
		t.Fatalf("unexpected namespaces.txt: %s", content)
	}

	if !strings.Contains(logs.String(), "no gather namespace found") {
		t.Fatalf("expected log about missing namespaces: %s", logs.String())
	}
}

func TestDumpFailureDiagnosticsSkipsDescribeOnNoResourcesFound(t *testing.T) {
	fakeOC(t, `if [ "$1" = describe ] || [ "$1" = logs ]; then
  printf 'unexpected %s\n' "$*" >> "$COMMAND_RECORD"
  exit 1
fi
if [ "$1" = get ] && [ "$2" = ns ]; then
  printf '%s\n' 'openshift-must-gather-abc 2026-10-01T00:00:00Z'
  exit 0
fi
if [ "$1" = get ] && [ "$2" = pods ]; then
  printf '%s\n' 'No resources found in openshift-must-gather-abc namespace.'
  exit 0
fi
if [ "$1" = get ] && [ "$2" = events ]; then
  printf '%s\n' 'LAST SEEN   TYPE   REASON'
  exit 0
fi
exit 0
`)
	record := filepath.Join(t.TempDir(), "bad-calls.txt")
	t.Setenv("COMMAND_RECORD", record)
	dest := t.TempDir()
	since := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	DumpFailureDiagnostics(context.Background(), dest, since, nil, testTimeout,
		func(string, ...interface{}) {})

	diagDir := filepath.Join(dest, failureDiagnosticsDir)
	podsPath := filepath.Join(diagDir, "openshift-must-gather-abc-pods.txt")
	content, err := os.ReadFile(podsPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(content), "No resources found") {
		t.Fatalf("pods dump missing empty-namespace note: %s", content)
	}

	entries, err := os.ReadDir(diagDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(name, "-describe.txt") && strings.Contains(name, "-pod-") {
			t.Fatalf("unexpected pod describe file written: %s", name)
		}

		if strings.Contains(name, "-logs.txt") {
			t.Fatalf("unexpected pod logs file written: %s", name)
		}
	}

	if _, err := os.Stat(record); err == nil {
		bad, readErr := os.ReadFile(record)
		if readErr != nil {
			t.Fatal(readErr)
		}

		t.Fatalf("describe/logs should not have been called: %s", bad)
	}
}

func TestDumpFailureDiagnosticsWithPodsAndNode(t *testing.T) {
	fakeOC(t, `if [ "$1" = get ] && [ "$2" = ns ]; then
  printf '%s\n' 'openshift-must-gather-abc 2026-10-01T00:00:00Z'
  exit 0
fi
if [ "$1" = get ] && [ "$2" = pods ]; then
  printf '%s\n' 'NAME READY STATUS' 'gather-pod 1/1 Running'
  exit 0
fi
if [ "$1" = get ] && [ "$2" = events ]; then
  printf '%s\n' 'event-line'
  exit 0
fi
if [ "$1" = describe ] && [ "$2" = pod ]; then
  printf 'pod-describe\n'
  exit 0
fi
if [ "$1" = describe ] && [ "$2" = node ]; then
  printf 'node-describe\n'
  exit 0
fi
if [ "$1" = logs ]; then
  printf 'pod-logs\n'
  exit 0
fi
exit 1
`)
	dest := t.TempDir()
	since := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	DumpFailureDiagnostics(context.Background(), dest, since, []string{"worker-0"}, testTimeout,
		func(string, ...interface{}) {})

	diagDir := filepath.Join(dest, failureDiagnosticsDir)
	for _, name := range []string{
		"namespaces.txt",
		"openshift-must-gather-abc-pods.txt",
		"openshift-must-gather-abc-events.txt",
		"openshift-must-gather-abc-pod-gather-pod-describe.txt",
		"openshift-must-gather-abc-pod-gather-pod-logs.txt",
		"node-worker-0-describe.txt",
	} {
		if _, err := os.Stat(filepath.Join(diagDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}
