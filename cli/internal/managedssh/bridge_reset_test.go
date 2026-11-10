package managedssh

import (
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

// resetBridge runs BridgeSSH against an sshd that sends part of a reply and then RST-closes. clientEOF makes the
// client finish sending first (a normal exit); otherwise it stays open, still waiting for the rest of the reply.
func resetBridge(t *testing.T, clientEOF bool) error {
	t.Helper()
	if len(closeResets) == 0 { // the Windows reset error is already listed; a Unix host lists none
		closeResets = []error{syscall.ECONNRESET}
		defer func() { closeResets = nil }()
	}
	previous := resetGrace
	resetGrace = 500 * time.Millisecond
	defer func() { resetGrace = previous }()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			return
		}
		_, _ = connection.Write([]byte("partial reply"))
		time.Sleep(100 * time.Millisecond) // the client's EOF, when there is one, lands first
		_ = connection.SetLinger(0)        // close sends RST
		_ = connection.Close()
	}()
	bridgeSide, clientSide := tcpPair(t)
	defer clientSide.Close()
	go func() { _, _ = io.Copy(io.Discard, clientSide) }()
	if clientEOF {
		_ = clientSide.CloseWrite()
	}
	_, err = BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
	return err
}

// A connection reset after the client finished sending is how Windows OpenSSH ends a session; the same reset
// while the client still waits for data is a truncated session and must be audited as a failure.
func TestConnectionResetIsCleanOnlyAfterTheClientFinished(t *testing.T) {
	if err := resetBridge(t, true); err != nil {
		t.Fatalf("reset after the client's EOF: %v, want a clean end", err)
	}
	if err := resetBridge(t, false); err == nil {
		t.Fatal("a reset while the client was still waiting for data was reported as a clean end")
	}
}
