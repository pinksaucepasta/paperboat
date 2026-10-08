//go:build linux

package machineguard

import (
	"errors"
	"net"

	"testing"
)

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
