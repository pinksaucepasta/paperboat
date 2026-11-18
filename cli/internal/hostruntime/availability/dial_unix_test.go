//go:build darwin || linux

package availability

import (
	"net"
	"os"
	"syscall"
	"testing"
)

type shutdownConn struct {
	net.Conn
	err error
}

func (c shutdownConn) CloseWrite() error { return c.err }

// A service that answers and closes before the client's shutdown makes macOS return ENOTCONN, which is not a
// failure (TestHostClientAppliesVersionZeroAndRejectsMismatchedResponse failed on it, 2026-11-18).
func TestClosingTheWriteHalfOfAnAlreadyClosedConnectionIsNotAFailure(t *testing.T) {
	notConnected := &net.OpError{Op: "close", Net: "unix", Err: os.NewSyscallError("shutdown", syscall.ENOTCONN)}
	if err := closeAvailabilityHostServiceWrite(shutdownConn{err: notConnected}); err != nil {
		t.Fatalf("ENOTCONN = %v", err)
	}
	if err := closeAvailabilityHostServiceWrite(shutdownConn{err: syscall.EPIPE}); err == nil {
		t.Fatal("another shutdown error was swallowed")
	}
}
