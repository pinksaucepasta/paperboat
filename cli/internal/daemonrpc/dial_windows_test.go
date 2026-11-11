//go:build windows

package daemonrpc

import (
	"os/user"
	"testing"
)

func TestDaemonPipeAddressUsesScopedDefault(t *testing.T) {
	got, err := daemonPipeAddress("")
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultSocketAddress(); got != want || got == DefaultWindowsNamedPipe {
		t.Fatalf("default address=%q want scoped %q", got, want)
	}
}

// The default pipe is named for the SID of the account the process runs as, so a process elevated as a different
// account addresses that account's pipe and never the enrolled user's (audit C #25); dial then verifies exclusivity.
func TestDefaultPipeIsNamedForTheCallersSID(t *testing.T) {
	t.Setenv("PAPERBOAT_DAEMON_PIPE", "")
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := defaultSocketAddress(), DefaultWindowsNamedPipe+"-"+current.Uid; got != want {
		t.Fatalf("default pipe %q, want %q", got, want)
	}
	t.Setenv("PAPERBOAT_DAEMON_PIPE", `\\.\pipe\other`)
	if got := defaultSocketAddress(); got != `\\.\pipe\other` {
		t.Fatalf("explicit pipe override ignored: %q", got)
	}
}
