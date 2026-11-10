//go:build !windows

package managedssh

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// On Unix a connection reset is still a failure (the Windows exception is bridge_reset_windows.go).
func TestUnixConnectionResetStaysAFailure(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	if err := normalizeBridgeError(reset); err == nil {
		t.Fatal("normalizeBridgeError(ECONNRESET) = nil on a Unix host, want the error")
	}
}
