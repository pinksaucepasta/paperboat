package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

// resetBridge runs BridgeSSH against an sshd that sends "reply" and then RST-closes. order says when the client
// finishes sending relative to the reply:
//   - "after": the client got the whole reply, then left (EOF), then sshd reset: how a Windows session ends
//   - "before": the client left first, sshd still sent and was cut: a download cut short
//   - "never": the client stays, waiting for the rest: an upload or download cut short
func resetBridge(t *testing.T, order string) error {
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
	sendReply, reset := make(chan struct{}), make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			return
		}
		<-sendReply
		_, _ = connection.Write([]byte("reply"))
		<-reset
		_ = connection.SetLinger(0) // close sends RST
		_ = connection.Close()
	}()
	bridgeSide, clientSide := tcpPair(t)
	defer clientSide.Close()
	got := make(chan struct{})
	go func() {
		buffer := make([]byte, 5)
		if _, readErr := io.ReadFull(clientSide, buffer); readErr == nil {
			close(got)
		}
		_, _ = io.Copy(io.Discard, clientSide)
	}()
	go func() {
		switch order {
		case "after":
			close(sendReply)
			<-got
			_ = clientSide.CloseWrite()
			time.Sleep(50 * time.Millisecond)
		case "before":
			_ = clientSide.CloseWrite()
			time.Sleep(50 * time.Millisecond)
			close(sendReply)
			<-got
		default:
			close(sendReply)
			<-got
		}
		close(reset)
	}()
	_, err = BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
	return err
}

// A connection reset after the client had everything and left is how Windows OpenSSH ends a session; the same reset
// while the client still waits for data, or after a client that left while sshd was still sending (a download
// cut short), is a truncated session and must be audited as a failure.
func TestConnectionResetIsCleanOnlyWhenBothDirectionsFinished(t *testing.T) {
	if err := resetBridge(t, "after"); err != nil {
		t.Fatalf("reset after the client had the reply and left: %v, want a clean end", err)
	}
	for _, order := range []string{"before", "never"} {
		if err := resetBridge(t, order); err == nil {
			t.Fatalf("a reset (%s) was reported as a clean end", order)
		}
	}
}

func TestCloseWriteFailureIgnoresOnlyTheBridgesOwnClose(t *testing.T) {
	refused := &net.OpError{Op: "close", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, c := range []struct {
		name     string
		err      error
		finished bool
		want     bool // reported as a failure
	}{
		{"no error", nil, false, false},
		{"closed after finish", net.ErrClosed, true, false},
		{"closed, bridge had not finished", net.ErrClosed, false, true},
		{"real failure after finish", refused, true, true},
		{"real failure", refused, false, true},
	} {
		if got := closeWriteFailure(c.err, c.finished) != nil; got != c.want {
			t.Errorf("%s: failure=%v, want %v", c.name, got, c.want)
		}
	}
}

// finLostConn fails CloseWrite (the FIN) with an error that is not a closed-connection one.
type finLostConn struct{ net.Conn }

func (finLostConn) CloseWrite() error { return errors.New("fin lost") }

// A FIN that never reached sshd leaves the command waiting for input that never ends: a failure, not a clean
// session, unless the bridge's own close made it fail.
func TestBridgeSSHFailedFINToSSHDIsAFailure(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
			defer connection.Close()
			_, _ = io.Copy(io.Discard, connection)
		}
	}()
	previous := dialSSHD
	dialSSHD = func(ctx context.Context, address string) (net.Conn, error) {
		connection, dialErr := previous(ctx, address)
		return finLostConn{connection}, dialErr
	}
	defer func() { dialSSHD = previous }()
	bridgeSide, clientSide := tcpPair(t)
	defer clientSide.Close()
	_ = clientSide.CloseWrite() // a clean copy: the client finished sending
	_, err = BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
	if err == nil {
		t.Fatal("a FIN that failed to reach sshd was reported as a clean session")
	}
}

// A failed FIN towards the client is a failure unless the client had already left (its EOF).
func TestBridgeSSHFailedFINToTheClientIsAFailureUnlessItLeft(t *testing.T) {
	previous := resetGrace
	resetGrace = 300 * time.Millisecond
	defer func() { resetGrace = previous }()
	for _, clientLeaves := range []bool{true, false} {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
				_, _ = connection.Write([]byte("done"))
				_ = connection.Close()
			}
		}()
		bridgeSide, clientSide := tcpPair(t)
		go func() { _, _ = io.Copy(io.Discard, clientSide) }()
		if clientLeaves {
			_ = clientSide.CloseWrite()
		}
		_, err = BridgeSSH(t.Context(), finLostConn{bridgeSide}, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
		_ = clientSide.Close()
		_ = listener.Close()
		if clientLeaves && err != nil {
			t.Fatalf("client left, FIN to it failed: %v, want a clean session", err)
		}
		if !clientLeaves && err == nil {
			t.Fatal("a FIN that failed towards a client that never left was reported as a clean session")
		}
	}
}
