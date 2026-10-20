//go:build !windows

package service

import (
	"strings"
	"testing"
)

// A child of a service must not inherit the manager's notification socket: the manager rejects its
// messages as coming from a process other than the main one and logs every one.
func TestExecRunnerChildrenDoNotInheritTheServiceManagerNotificationEnvironment(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/run/systemd/notify")
	t.Setenv("WATCHDOG_USEC", "30000000")
	t.Setenv("WATCHDOG_PID", "1")
	t.Setenv("PAPERBOAT_TEST_KEEP", "kept")
	output, err := ExecRunner{}.Output(t.Context(), "env")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"NOTIFY_SOCKET=", "WATCHDOG_USEC=", "WATCHDOG_PID="} {
		if strings.Contains(output, name) {
			t.Fatalf("child inherited %s", name)
		}
	}
	if !strings.Contains(output, "PAPERBOAT_TEST_KEEP=kept") {
		t.Fatalf("child lost its ordinary environment: %q", output)
	}
	explicit, err := ExecRunner{Environment: []string{"A=1"}}.Output(t.Context(), "env")
	if err != nil || strings.TrimSpace(explicit) != "A=1" {
		t.Fatalf("explicit environment changed: %q %v", explicit, err)
	}
}
