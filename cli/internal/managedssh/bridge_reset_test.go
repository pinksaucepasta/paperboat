package managedssh

import (
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
