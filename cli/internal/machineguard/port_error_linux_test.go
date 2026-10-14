//go:build linux

package machineguard

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"

	"testing"
)

type closeTrackingListener struct {
	net.Listener
	closeErr   error
	closeCalls int
}

func (l *closeTrackingListener) Close() error {
	l.closeCalls++
	return errors.Join(l.Listener.Close(), l.closeErr)
}

func TestProtectedListenerPortConflictClassification(t *testing.T) {
	first, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	occupied, err := net.Listen("tcp4", first.Addr().String())
	err = classifyProtectedBindError(err)
	if occupied != nil {
		occupied.Close()
		t.Fatal("occupied listener acquired")
	}
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("occupied port: %v", err)
	}
	_, err = net.Listen("tcp4", net.JoinHostPort("invalid address", "1234"))
	err = classifyProtectedBindError(err)
	if err == nil || errors.Is(err, ErrPortInUse) {
		t.Fatalf("non-bind failure misclassified: %v", err)
	}
}

func TestShutdownListenerClosesOwnedSocket(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err = shutdownListener(listener); err != nil && !errors.Is(err, unix.EPERM) {
		t.Fatal(err)
	}
	if _, err = listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener remained open after shutdown: %v", err)
	}
}

func TestShutdownListenerClosesUnsupportedOwnedListener(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closeCause := errors.New("listener close failed")
	listener := &closeTrackingListener{Listener: base, closeErr: closeCause}
	if err = shutdownListener(listener); !errors.Is(err, closeCause) {
		t.Fatalf("shutdown error = %v, want owned close cause", err)
	}
	if listener.closeCalls != 1 {
		t.Fatalf("owned listener Close calls = %d, want one", listener.closeCalls)
	}
	if _, err = base.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unsupported listener remained open: %v", err)
	}
}
