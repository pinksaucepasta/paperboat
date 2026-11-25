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
	t.Setenv("WATCHDOG_USEC", "300000000")
	t.Setenv("WATCHDOG_PID", "1")
	t.Setenv("PAPERBOAT_TEST_KEEP", "kept")
	// Output keeps 8 KiB, less than a developer's or CI runner's whole environment: ask for the four values only.
	t.Setenv("PAPERBOAT_TEST_LONG", strings.Repeat("x", 16<<10))
	output, err := ExecRunner{}.Output(t.Context(), "sh", "-c", `echo "keep=$PAPERBOAT_TEST_KEEP notify=${NOTIFY_SOCKET-} usec=${WATCHDOG_USEC-} pid=${WATCHDOG_PID-}"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output) != "keep=kept notify= usec= pid=" {
		t.Fatalf("child environment: %q, want the ordinary value kept and no service manager notification values", output)
	}
	explicit, err := ExecRunner{Environment: []string{"A=1"}}.Output(t.Context(), "env")
	if err != nil || strings.TrimSpace(explicit) != "A=1" {
		t.Fatalf("explicit environment changed: %q %v", explicit, err)
	}
}
