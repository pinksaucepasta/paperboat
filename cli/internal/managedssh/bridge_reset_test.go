package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// resetBridge runs BridgeSSH against an sshd that sends "reply" and then RST-closes. order says when the client
// finishes sending relative to the reply:
//   - "after": the client got the whole reply, then left (EOF), then sshd reset: how a Windows session ends
//   - "stdin-first": the client half-closed its sending side first (`ssh host cmd < file`: all input sent), then
//     got the whole reply, then sshd reset: also a complete session
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
		case "stdin-first":
			_ = clientSide.CloseWrite()
			close(sendReply)
			<-got
		default:
			close(sendReply)
			<-got
		}
		time.Sleep(50 * time.Millisecond)
		close(reset)
	}()
	_, err = BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
	return err
}

// A connection reset after the client had everything is how Windows OpenSSH ends a session, whether the client
// half-closed its input first or left afterwards; the same reset while the client still waits for data is a
// truncated session and must be audited as a failure.
func TestConnectionResetIsCleanOnlyWhenTheClientHadItsFullReply(t *testing.T) {
	for _, order := range []string{"after", "stdin-first"} {
		if err := resetBridge(t, order); err != nil {
			t.Fatalf("reset after a complete reply (%s): %v, want a clean end", order, err)
		}
	}
	err := resetBridge(t, "never")
	if err == nil {
		t.Fatal("a reset while the client still waited for data was reported as a clean end")
	}
	// The failure names the direction and that the reset rule ran, for the diagnostics (types only are recorded).
	var from *fromSSHDError
	var unexcused *resetNotExcusedError
	if !errors.As(err, &from) || !errors.As(err, &unexcused) {
		t.Fatalf("the failure does not say which copier and rule it came from: %v", err)
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

// A failed CloseWrite (the FIN to sshd) is ignored only when the bridge had already closed the sshd socket and the
// error is what a closed socket reports.
func TestCloseWriteFailureIsIgnoredOnlyAfterTheBridgeClosed(t *testing.T) {
	// On Windows the closed-socket error is a net.OpError around a fresh error value, not net.ErrClosed.
	closedByUs := &net.OpError{Op: "close", Net: "tcp", Err: errors.New("use of closed network connection")}
	refused := &net.OpError{Op: "close", Net: "tcp", Err: os.NewSyscallError("shutdown", syscall.ECONNREFUSED)}
	for _, c := range []struct {
		name         string
		err          error
		closedBefore bool
		want         bool // reported as a failure
	}{
		{"no error", nil, false, false},
		{"closed by the bridge, typed", net.ErrClosed, true, false},
		{"closed by the bridge, fresh closed-socket error", closedByUs, true, false},
		{"refused after the bridge closed: a real failure", refused, true, true},
		{"unknown error after the bridge closed", errors.New("fin lost"), true, true},
		{"refused while sshd was open", refused, false, true},
		{"closed-socket error while sshd was still open (racing close)", closedByUs, false, true},
		{"fin lost while sshd was open", errors.New("fin lost"), false, true},
	} {
		if got := closeWriteFailure(c.err, c.closedBefore) != nil; got != c.want {
			t.Errorf("%s: failure=%v, want %v", c.name, got, c.want)
		}
	}
}

// closedAwareConn reports a closed-socket CloseWrite error only once Close has run, as a real socket does.
type closedAwareConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closedAwareConn) Close() error { err := c.Conn.Close(); c.closed.Store(true); return err }
func (c *closedAwareConn) CloseWrite() error {
	if c.closed.Load() {
		return &net.OpError{Op: "close", Net: "tcp", Err: errors.New("use of closed network connection")}
	}
	return nil
}

// sshd ends first, the bridge closes sshd and then the stream, and to_sshd's CloseWrite meets the closed socket: that
// is the bridge's own close, a clean session (the ordering the closed flag has to get right).
func TestBridgeSSHOwnCloseRacingCloseWriteIsClean(t *testing.T) {
	previous := dialSSHD
	dialSSHD = func(ctx context.Context, address string) (net.Conn, error) {
		connection, dialErr := previous(ctx, address)
		return &closedAwareConn{Conn: connection}, dialErr
	}
	defer func() { dialSSHD = previous }()
	for range 50 { // the order of the two closes against the copier's wake-up is a race: repeat
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
		_, err = BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
		_ = clientSide.Close()
		_ = listener.Close()
		if err != nil {
			t.Fatalf("a session sshd ended, closed by the bridge, reported %v", err)
		}
	}
}

// What a real closed TCP socket reports from CloseWrite is what resultOfOwnClose must name: measured on Windows 11 for a
// plain close, a close after the peer's FIN or RST, and a CloseWrite racing Close: always an *net.OpError (Op "close") around
// net.ErrClosed, never a WSA errno. A live socket whose peer reset reports nothing.
func TestResultOfOwnCloseNamesWhatARealClosedSocketReports(t *testing.T) {
	bridgeSide, clientSide := tcpPair(t)
	defer clientSide.Close()
	if err := bridgeSide.Close(); err != nil {
		t.Fatal(err)
	}
	err := bridgeSide.CloseWrite()
	if err == nil {
		t.Fatal("CloseWrite on a closed socket reported nothing")
	}
	if !resultOfOwnClose(err) {
		t.Fatalf("CloseWrite after our own Close = %T %v (is ErrClosed: %v), not named as the result of our own close", err, err, errors.Is(err, net.ErrClosed))
	}
}
