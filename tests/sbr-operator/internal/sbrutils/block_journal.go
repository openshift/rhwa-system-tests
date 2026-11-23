package sbrutils

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BlockHost runs commands through a connection prepared before disruption.
type BlockHost interface {
	Run(context.Context, time.Duration, string) (string, error)
}

const blockHostCall = 15 * time.Second

// BlockJournalFault owns a reversible fault on the host journal directory.
type BlockJournalFault struct {
	host          BlockHost
	temporaryPath string
	originalFlags string
	added         bool
	prepared      bool
}

// NewBlockJournalFault reserves an owned temporary path without changing the host.
func NewBlockJournalFault(host BlockHost, owner string) (*BlockJournalFault, error) {
	if host == nil || !validBlockOwner(owner) {
		return nil, fmt.Errorf("journal fault requires a host and a valid owner")
	}

	return &BlockJournalFault{host: host, temporaryPath: "/var/lib/medik8s/state/.sbr-test-" + owner}, nil
}

// Prepare saves the original directory flags and checks native prerequisites.
func (fault *BlockJournalFault) Prepare(ctx context.Context) error {
	output, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(
		"set -eu\ncommand -v chattr >/dev/null\ncommand -v lsattr >/dev/null\ntest -d /var/lib/medik8s/state\n"+
			"test ! -e "+blockShellQuote(fault.temporaryPath)+"\nlsattr -d /var/lib/medik8s/state"))
	if err != nil {
		return &BlockPrerequisiteError{Reason: fmt.Sprintf("journal directory or native commands unavailable: %v", err)}
	}

	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != "/var/lib/medik8s/state" || strings.ContainsAny(fields[0], "\n\r '") {
		return fmt.Errorf("unexpected journal directory flags %q", output)
	}

	fault.originalFlags = fields[0]
	if strings.Contains(fault.originalFlags, "i") {
		return &BlockPrerequisiteError{Reason: "the journal directory already has an immutable flag"}
	}

	fault.prepared = true

	return nil
}

// Activate prevents directory writes without reading journal contents.
func (fault *BlockJournalFault) Activate(ctx context.Context) error {
	if !fault.prepared {
		return fmt.Errorf("prepare the journal fault before activation")
	}

	if !strings.Contains(fault.originalFlags, "i") {
		// Set ownership before dispatch so cleanup covers a lost SSH acknowledgement.
		fault.added = true
		if _, err := fault.host.Run(ctx, blockHostCall, "sudo chattr +i /var/lib/medik8s/state"); err != nil {
			return fmt.Errorf("set immutable journal directory: %w", err)
		}
	}

	return fault.Confirm(ctx)
}

// Confirm checks the immutable flag and an owned disposable write.
func (fault *BlockJournalFault) Confirm(ctx context.Context) error {
	if !fault.prepared {
		return fmt.Errorf("prepare the journal fault before confirmation")
	}

	script := "set -eu\nflags=$(lsattr -d /var/lib/medik8s/state)\nflags=${flags%% *}\n" +
		"case \"$flags\" in *i*) ;; *) exit 1 ;; esac\n" +
		"if (set -C; : > " + blockShellQuote(fault.temporaryPath) + ") 2>/dev/null; then\n" +
		"rm -- " + blockShellQuote(fault.temporaryPath) + "\nexit 1\nfi\n" +
		"test ! -e " + blockShellQuote(fault.temporaryPath) + "\n"
	_, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script))
	if err != nil {
		return fmt.Errorf("journal directory access fault is not confirmed: %w", err)
	}

	return nil
}

// Restore removes the owned flag and checks the original directory access.
func (fault *BlockJournalFault) Restore(ctx context.Context) error {
	if !fault.prepared {
		return nil
	}

	if fault.added {
		if _, err := fault.host.Run(ctx, blockHostCall, "sudo chattr -i /var/lib/medik8s/state"); err != nil {
			return fmt.Errorf("restore journal directory flag: %w", err)
		}
	}

	script := "set -eu\nflags=$(lsattr -d /var/lib/medik8s/state)\nflags=${flags%% *}\n" +
		"test \"$flags\" = " + blockShellQuote(fault.originalFlags) + "\n"
	if !strings.Contains(fault.originalFlags, "i") {
		script += "rm -f -- " + blockShellQuote(fault.temporaryPath) + "\n" +
			"(set -C; : > " + blockShellQuote(fault.temporaryPath) + ")\nrm -- " + blockShellQuote(fault.temporaryPath) + "\n"
	}

	if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
		return fmt.Errorf("check restored journal directory: %w", err)
	}

	fault.added = false

	return nil
}

func blockShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func validBlockOwner(owner string) bool {
	if len(owner) == 0 || len(owner) > 48 {
		return false
	}

	for _, character := range owner {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}

	return true
}
