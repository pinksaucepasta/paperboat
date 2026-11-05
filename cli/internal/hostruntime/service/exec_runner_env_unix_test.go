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
	// A large environment ahead of the kept variable must not matter.
	t.Setenv("PAPERBOAT_TEST_LARGE", strings.Repeat("x", 16<<10))
	t.Setenv("PAPERBOAT_TEST_KEEP", "kept")
	// The child prints only the variables under test: a whole `env` dump can pass ExecRunner's 8 KiB output
	// cap on a large environment (a long CI -run pattern), which hid the kept variable.
	output, err := ExecRunner{}.Output(t.Context(), "sh", "-c", `printf '%s|%s|%s|%s' "${NOTIFY_SOCKET-unset}" "${WATCHDOG_USEC-unset}" "${WATCHDOG_PID-unset}" "${PAPERBOAT_TEST_KEEP-unset}"`)
	if err != nil {
		t.Fatal(err)
	}
	if output != "unset|unset|unset|kept" {
		t.Fatalf("child environment = %q, want the notification variables unset and the ordinary one kept", output)
	}
	explicit, err := ExecRunner{Environment: []string{"A=1"}}.Output(t.Context(), "env")
	if err != nil || strings.TrimSpace(explicit) != "A=1" {
		t.Fatalf("explicit environment changed: %q %v", explicit, err)
	}
}
