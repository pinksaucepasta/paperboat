//go:build windows

package managedssh

import (
	"net"
	"os"
	"syscall"
	"testing"
)

// The audit outcome of a Windows target's session was `failed` for every normal end: Windows OpenSSH's RST-close surfaces as WSAECONNRESET through
// OpError and SyscallError, and normalizeBridgeError only knew EOF and closed-pipe errors.
func TestWindowsConnectionResetAtTheEndOfASessionIsNotAFailure(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", syscall.Errno(10054))}
	if err := normalizeBridgeError(reset); err != nil {
		t.Fatalf("normalizeBridgeError(WSAECONNRESET)=%v, want nil", err)
	}
	if err := normalizeBridgeError(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", syscall.Errno(10053))}); err == nil {
		t.Fatal("a different Winsock error (aborted) must still be a failure")
	}
}
