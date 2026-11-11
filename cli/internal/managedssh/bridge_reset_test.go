package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
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
//   - "abort": the client got the whole reply and then aborted its connection (RST) instead of a clean close
//   - "noisy": as "after", but the bridge's CloseWrite towards the client reports an unrelated closed-socket error
//   - "never": the client stays, waiting for the rest: an upload or download cut short
func resetBridge(t *testing.T, order string) error {
	t.Helper()
	if len(resetErrors()) == 0 { // the Windows reset error is already listed; a Unix host lists none
		defer overrideCloseResets([]error{syscall.ECONNRESET})()
	}
	defer overrideResetGrace(500 * time.Millisecond)()
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
		case "abort":
			close(sendReply)
			<-got
			_ = clientSide.SetLinger(0)
			_ = clientSide.Close()
		case "noisy":
			close(sendReply)
			<-got
			_ = clientSide.CloseWrite()
		default:
			close(sendReply)
			<-got
		}
		time.Sleep(50 * time.Millisecond)
		close(reset)
	}()
	var stream io.ReadWriteCloser = bridgeSide
	switch order {
	case "noisy":
		stream = noisyCloseWriteConn{bridgeSide}
	case "abort":
		stream = abortedStream{bridgeSide}
	}
	_, err = BridgeSSH(t.Context(), stream, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
	return err
}

// abortedStream reports a client abort the way a native QUIC stream does: a typed stream error, never a TCP reset. A
// TCP RST on Windows arrives as WSAECONNRESET, which closeResets lists as a forgivable reset: the production client
// streams are QUIC and have no such value, so the tests model the abort with one that is not a reset on any OS.
// It holds the connection in a field, not embedded: embedding would promote TCPConn's WriteTo and io.Copy would use it
// and bypass Read, so the abort would never be reported as this error.
type abortedStream struct{ conn *net.TCPConn }

var errStreamAborted = errors.New("peer aborted the stream")

func (a abortedStream) Read(b []byte) (int, error) {
	n, err := a.conn.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		err = errStreamAborted
	}
	return n, err
}

func (a abortedStream) Close() error      { return a.conn.Close() }
func (a abortedStream) CloseWrite() error { return a.conn.CloseWrite() }

func (a abortedStream) Write(b []byte) (int, error) {
	n, err := a.conn.Write(b)
	if err != nil {
		err = errStreamAborted
	}
	return n, err
}

// noisyCloseWriteConn fails CloseWrite with a closed-socket error that is a fresh value, as a native stream does once
// the client has gone; measured beside the reset on a Windows carrier.
type noisyCloseWriteConn struct{ *net.TCPConn }

func (noisyCloseWriteConn) CloseWrite() error {
	return &net.OpError{Op: "close", Net: "tcp", Err: errors.New("use of closed network connection")}
}

// Windows OpenSSH ends a connection with an RST where a Unix sshd sends a FIN, so a reset READ from sshd is its end of
// output and is recorded like the Unix EOF, whatever the client does around it: leaves after the full reply, half-closes
// first, its stream's CloseWrite reports noise, or it is still waiting. (A sshd that dies mid-transfer ends the same
// way on both: the bridge cannot tell it from a normal end, on Unix either.) A client that aborts its stream is not
// forgiven on either OS (CloseWrite towards an aborted client fails and its EOF never shows), so it is not in this list.
func TestWindowsReadResetIsHowSSHDEnds(t *testing.T) {
	for _, order := range []string{"after", "stdin-first", "noisy", "never"} {
		if err := resetBridge(t, order); err != nil {
			t.Fatalf("sshd's reset (%s): %v, want the clean end a Unix sshd's EOF gives", order, err)
		}
	}
}

// A client that aborts its stream after the full reply is recorded as failed, as on Unix (22bb0ac26 and before): the
// bridge reports the failed FIN towards a client that never showed a clean EOF.
func TestClientAbortAfterTheReplyIsRecordedLikeOnUnix(t *testing.T) {
	if err := resetBridge(t, "abort"); err == nil {
		t.Fatal("a client abort was recorded as a clean end; Unix records it as failed")
	}
}

// On Unix a reset from sshd is not an end: closeResets is empty there (bridge_reset_other.go), so it stays a failure.
func TestUnixResetFromSSHDStaysAFailure(t *testing.T) {
	if len(resetErrors()) != 0 {
		t.Skip("closeResets lists the Windows reset")
	}
	defer overrideResetGrace(200 * time.Millisecond)()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
			_, _ = connection.Write([]byte("reply"))
			time.Sleep(50 * time.Millisecond)
			_ = connection.SetLinger(0)
			_ = connection.Close()
		}
	}()
	bridgeSide, clientSide := tcpPair(t)
	defer clientSide.Close()
	go func() { _, _ = io.Copy(io.Discard, clientSide) }()
	if _, err := BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second); err == nil {
		t.Fatal("a reset from sshd was recorded as a clean end on a Unix host")
	}
}

// A client that aborts mid-upload while sshd is still running is a cut session on every OS: sshd has not ended, so
// nothing forgives the client's error.
func TestClientAbortWhileSSHDIsStillRunningIsAFailure(t *testing.T) {
	if len(resetErrors()) == 0 { // the Windows reset error is already listed; a Unix host lists none
		defer overrideCloseResets([]error{syscall.ECONNRESET})()
	}
	defer overrideResetGrace(200 * time.Millisecond)()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
			defer connection.Close()
			_, _ = io.Copy(io.Discard, connection) // sshd keeps reading the upload
		}
	}()
	bridgeSide, clientSide := tcpPair(t)
	go func() {
		_, _ = clientSide.Write([]byte("part of an upload"))
		time.Sleep(100 * time.Millisecond)
		_ = clientSide.SetLinger(0)
		_ = clientSide.Close() // the abort
	}()
	if _, err := BridgeSSH(t.Context(), abortedStream{bridgeSide}, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second); err == nil {
		t.Fatal("a client that aborted mid-upload, sshd still running, was recorded as a clean end")
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
	defer overrideDialSSHD(func(ctx context.Context, address string) (net.Conn, error) {
		connection, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		return finLostConn{connection}, dialErr
	})()
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
	defer overrideResetGrace(300 * time.Millisecond)()
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
		name    string
		err     error
		closing bool
		want    bool // reported as a failure
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
		if got := closeWriteFailure(c.err, c.closing) != nil; got != c.want {
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
	defer overrideDialSSHD(func(ctx context.Context, address string) (net.Conn, error) {
		connection, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		return &closedAwareConn{Conn: connection}, dialErr
	})()
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

// The mid-upload abort the coordinator named: the client aborts a push (scp/rsync), sshd then sees the end of its input
// and exits within the grace period. It stays failed on every OS (82e5c6ac7 turned it into closed everywhere; this is
// the test that would have caught it), with and without the Windows reset listed.
func TestMidUploadAbortStaysFailedEvenWhenSSHDExitsPromptly(t *testing.T) {
	for _, windows := range []bool{false, true} {
		resets := []error(nil)
		if windows {
			resets = []error{syscall.ECONNRESET}
		}
		restoreResets, restoreGrace := overrideCloseResets(resets), overrideResetGrace(500*time.Millisecond)
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
				_, _ = io.Copy(io.Discard, connection) // reads the upload until its input ends
				_ = connection.Close()                 // then exits cleanly (FIN)
			}
		}()
		bridgeSide, clientSide := tcpPair(t)
		go func() {
			_, _ = clientSide.Write([]byte("part of an upload"))
			time.Sleep(100 * time.Millisecond)
			_ = clientSide.SetLinger(0)
			_ = clientSide.Close() // the abort
		}()
		_, err = BridgeSSH(t.Context(), abortedStream{bridgeSide}, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
		restoreGrace()
		restoreResets()
		_ = listener.Close()
		if err == nil {
			t.Fatalf("a mid-upload client abort, sshd exiting at once, was recorded closed (windows reset listed: %v)", windows)
		}
	}
}

// A client that aborts mid-download (sshd is still sending) is a cut session on every OS: the write towards the
// aborted client fails, which is a write error, never sshd's end of output (only a reset READ from sshd is that).
func TestMidDownloadClientAbortIsFailedOnEveryOS(t *testing.T) {
	for _, windows := range []bool{false, true} {
		resets := []error(nil)
		if windows {
			resets = []error{syscall.ECONNRESET}
		}
		restoreResets, restoreGrace := overrideCloseResets(resets), overrideResetGrace(200*time.Millisecond)
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
				defer connection.Close()
				chunk := make([]byte, 64<<10)
				for { // sshd keeps sending the download until the connection goes
					if _, writeErr := connection.Write(chunk); writeErr != nil {
						return
					}
				}
			}
		}()
		bridgeSide, clientSide := tcpPair(t)
		go func() {
			_, _ = io.ReadFull(clientSide, make([]byte, 128<<10)) // some of the download arrives
			_ = clientSide.SetLinger(0)
			_ = clientSide.Close() // the abort
		}()
		_, err = BridgeSSH(t.Context(), abortedStream{bridgeSide}, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second)
		restoreGrace()
		restoreResets()
		_ = listener.Close()
		if err == nil {
			t.Fatalf("a client abort mid-download was recorded closed (windows reset listed: %v)", windows)
		}
	}
}

// closeGatedConn holds CloseWrite until Close has run, then reports a closed socket: the client's EOF and sshd's end
// arrive together and the bridge's own close lands inside to_sshd's CloseWrite.
type closeGatedConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *closeGatedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}
func (c *closeGatedConn) CloseWrite() error {
	<-c.closed
	return &net.OpError{Op: "close", Net: "tcp", Err: errors.New("use of closed network connection")}
}

// A client that leaves just as sshd ends makes the bridge's own sshd close meet to_sshd's CloseWrite in flight. That is
// a clean session, not a failed FIN (it failed about 4% of runs when the bridge only trusted a flag read before the call).
func TestBridgeSSHOwnCloseInsideCloseWriteIsClean(t *testing.T) {
	defer overrideDialSSHD(func(ctx context.Context, address string) (net.Conn, error) {
		connection, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		return &closeGatedConn{Conn: connection, closed: make(chan struct{})}, dialErr
	})()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		if connection, acceptErr := listener.AcceptTCP(); acceptErr == nil {
			_, _ = connection.Write([]byte("done"))
			_ = connection.Close()
		}
	}()
	bridgeSide, clientSide := tcpPair(t)
	_ = clientSide.Close() // the client's EOF, so to_sshd reaches CloseWrite while sshd's end is still coming
	if _, err := BridgeSSH(t.Context(), bridgeSide, LoopbackTarget{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}, time.Second); err != nil {
		t.Fatalf("a session whose own close landed inside CloseWrite reported %v", err)
	}
}
