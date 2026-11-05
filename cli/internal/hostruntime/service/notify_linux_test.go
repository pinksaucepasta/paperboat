package service

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestProcessNotifierLifecycleAndWatchdog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "2000000")
	t.Setenv("WATCHDOG_PID", "")
	resetServiceManager(t)

	notifier, err := NewProcessNotifier()
	if err != nil {
		t.Fatal(err)
	}
	if notifier.WatchdogInterval() != time.Second {
		t.Fatalf("watchdog interval=%s", notifier.WatchdogInterval())
	}
	operations := []struct {
		call func() error
		want string
	}{
		{notifier.Starting, "STATUS=starting"},
		{notifier.Ready, "READY=1\nSTATUS=ready"},
		{notifier.Watchdog, "WATCHDOG=1"},
		{notifier.Draining, "STATUS=draining"},
		{notifier.Stopping, "STOPPING=1\nSTATUS=stopping"},
	}
	buffer := make([]byte, 512)
	for _, operation := range operations {
		if err := operation.call(); err != nil {
			t.Fatal(err)
		}
		if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		count, _, err := listener.ReadFromUnix(buffer)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(buffer[:count]); got != operation.want {
			t.Fatalf("notification=%q want=%q", got, operation.want)
		}
	}
	if err := notifier.Degraded("bad\nstatus"); err == nil {
		t.Fatal("accepted unsafe status")
	}
	if os.Getenv("NOTIFY_SOCKET") != "" || os.Getenv("WATCHDOG_USEC") != "" {
		t.Fatal("child processes would inherit the service manager's notification socket")
	}
	again, err := NewProcessNotifier()
	if err != nil || again.Starting() != nil {
		t.Fatalf("a second notifier in the process lost the socket: %v", err)
	}
}

// systemd's restart backoff is reset once the process has stayed ready for a minute, not at READY: a process
// that crashes right after start must keep backing off.
func TestProcessNotifierResetsRestartBackoffOnlyAfterStayingReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("WATCHDOG_USEC", "2000000")
	t.Setenv("WATCHDOG_PID", "")
	resetServiceManager(t)
	notifier, err := NewProcessNotifier()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	notifier.now = func() time.Time { return now }
	buffer := make([]byte, 512)
	receive := func(call func() error) string {
		t.Helper()
		if err := call(); err != nil {
			t.Fatal(err)
		}
		if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		count, _, err := listener.ReadFromUnix(buffer)
		if err != nil {
			t.Fatal(err)
		}
		return string(buffer[:count])
	}
	if got := receive(notifier.Watchdog); got != "WATCHDOG=1" {
		t.Fatalf("before ready: %q", got)
	}
	if got := receive(notifier.Ready); got != "READY=1\nSTATUS=ready" {
		t.Fatalf("ready: %q", got)
	}
	now = now.Add(59 * time.Second)
	if got := receive(notifier.Watchdog); got != "WATCHDOG=1" {
		t.Fatalf("59 s after ready: %q, want no reset yet", got)
	}
	now = now.Add(time.Second)
	if got := receive(notifier.Watchdog); got != "WATCHDOG=1\nRESTART_RESET=1" {
		t.Fatalf("60 s after ready: %q, want the restart backoff reset", got)
	}
	if got := receive(notifier.Watchdog); got != "WATCHDOG=1" {
		t.Fatalf("after the reset: %q, want it sent once", got)
	}
}

func resetServiceManager(t *testing.T) {
	t.Helper()
	serviceManager = sync.OnceValues(captureServiceManager)
	t.Cleanup(func() { serviceManager = sync.OnceValues(captureServiceManager) })
}

func TestProcessNotifierRejectsInvalidWatchdogConfiguration(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	t.Setenv("WATCHDOG_USEC", "invalid")
	t.Setenv("WATCHDOG_PID", os.Getenv("WATCHDOG_PID"))
	resetServiceManager(t)
	if _, err := NewProcessNotifier(); err == nil {
		t.Fatal("accepted invalid watchdog configuration")
	}
}
