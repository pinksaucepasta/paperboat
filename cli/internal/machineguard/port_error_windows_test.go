//go:build windows

package machineguard

import (
	"errors"
	"golang.org/x/sys/windows"
	"net"
	"os"
	"strconv"
	"testing"
)

func TestWindowsProtectedListenerClassifiesOccupiedPort(t *testing.T) {
	first, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := listenProtected(t.Context(), first.Addr().String(), "127.100.0.0/16")
	if second != nil {
		second.Close()
		t.Fatal("occupied port was acquired")
	}
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("occupied port did not preserve its typed error: %v", err)
	}
	if _, err := listenProtected(t.Context(), "invalid address:1234", "127.100.0.0/16"); err == nil || errors.Is(err, ErrPortInUse) {
		t.Fatalf("unrelated listener failure misclassified: %v", err)
	}
}

func TestWindowsProtectedListenerClassifiesOnlyBindAccessDenial(t *testing.T) {
	for _, tc := range []struct {
		operation   string
		unavailable bool
	}{{"bind", true}, {"setsockopt", false}, {"open", false}} {
		cause := &os.SyscallError{Syscall: tc.operation, Err: windows.WSAEACCES}
		err := windowsProtectedListenError(&net.OpError{Op: "listen", Net: "tcp4", Err: cause})
		if errors.Is(err, ErrPortInUse) != tc.unavailable {
			t.Fatalf("%s incorrectly classified: %v", tc.operation, err)
		}
		if !tc.unavailable && !errors.Is(err, windows.WSAEACCES) {
			t.Fatalf("%s lost original failure: %v", tc.operation, err)
		}
	}
	if errors.Is(windowsProtectedListenError(windows.WSAEACCES), ErrPortInUse) {
		t.Fatal("generic permission failure classified as port conflict")
	}
}

func TestWindowsProtectedListenerReservedPort(t *testing.T) {
	raw := os.Getenv("PB_TEST_RESERVED_TCP_PORT")
	if raw == "" {
		t.Skip("requires a read-only verified Windows excluded TCP port")
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid reserved test port")
	}
	listener, err := listenProtected(t.Context(), net.JoinHostPort("127.100.0.99", raw), "127.100.0.0/16")
	if listener != nil {
		listener.Close()
		t.Fatal("verified excluded port unexpectedly became available")
	}
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("reserved native port aborted independent browser routing: %v", err)
	}
}
