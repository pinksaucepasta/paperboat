//go:build windows

package daemonrpc

import (
	"context"
	"os/user"
	"testing"
	"time"
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

// A daemon pipe that accepts but never answers (a wedged daemon) fails Dial within dialTimeout even when the caller's
// context has no deadline (audit C #32; the Unix test is TestDialIsBoundedWithoutACallerDeadline).
func TestDialIsBoundedWithoutACallerDeadlineOnAPipe(t *testing.T) {
	original := dialTimeout
	dialTimeout = 500 * time.Millisecond
	t.Cleanup(func() { dialTimeout = original })
	path := testSocket(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	began := time.Now()
	if conn, err := Dial(context.Background(), path); err == nil {
		_ = conn.Close()
		t.Fatal("Dial reached a server that never answers")
	}
	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("Dial took %v", elapsed)
	}
}
