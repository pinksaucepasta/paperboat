//go:build windows

package deviceguard

import (
	"errors"
	"net"
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
