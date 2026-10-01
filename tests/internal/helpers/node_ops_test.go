package helpers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartKubeletSSHByIP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLUSTER_PROFILE_DIR", dir)
	t.Setenv("SHARED_DIR", dir)
	t.Setenv("PATH", dir)
	logPath := filepath.Join(dir, "commands")
	t.Setenv("SSH_COMMAND_LOG", logPath)

	for name, content := range map[string]string{
		"ssh-privatekey": "test fixture, not a real key",
		"oc":             "#!/bin/sh\nexit 1\n",
		"ssh": "#!/bin/sh\nfor arg do command=$arg; done\n" +
			"printf '%s\\n' \"$command\" >> \"$SSH_COMMAND_LOG\"\n" +
			"if [ \"$command\" = \"${SSH_FAIL_COMMAND:-}\" ]; then exit 1; fi\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() {
		if sshKeyPath != "" {
			if err := os.Remove(sshKeyPath); err != nil {
				t.Error(err)
			}
		}
	})

	for _, testCase := range []struct {
		name        string
		ip          string
		failCommand string
		want        string
		wantError   bool
	}{
		{"saved address", "192.0.2.1", "",
			"sudo systemctl unmask --runtime kubelet\nsudo systemctl daemon-reload\nsudo systemctl start kubelet\n", false},
		{"reload failure", "192.0.2.1", "sudo systemctl daemon-reload",
			"sudo systemctl unmask --runtime kubelet\nsudo systemctl daemon-reload\n", true},
		{"missing address", "", "", "", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("SSH_FAIL_COMMAND", testCase.failCommand)

			if err := os.WriteFile(logPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			err := StartKubeletSSHByIP(context.Background(), testCase.ip, time.Second)
			if (err != nil) != testCase.wantError {
				t.Fatalf("unexpected recovery error: %v", err)
			}

			commands, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}

			if strings.TrimSpace(string(commands)) != strings.TrimSpace(testCase.want) {
				t.Fatalf("unexpected command sequence: %q", commands)
			}
		})
	}
}
