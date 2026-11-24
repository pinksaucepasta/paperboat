package managedssh

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// RefuseUnserved tells the client, and the log, why its stream ends before sshd could answer.
func RefuseUnserved(stream io.Writer, reason string, cause error) {
	diagnosticlog.TryWarn("SSH stream ended before its SSH server answered", "reason", reason, "error", cause)
	streamauth.WriteSSHRefusal(stream, reason)
}

// answerStream is the client's stream until sshd's first byte: closing it before then first says why, so a client
// never sees a connection that ends without a word. Prior art: tailscale ssh/tailssh errBanner.
type answerStream struct {
	io.ReadWriteCloser
	reason func() string

	mu       sync.Mutex
	answered bool
	refused  bool
}

func (s *answerStream) refuse(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answered {
		return
	}
	s.answered, s.refused = true, true
	// A client that does not read must not hold up a revocation: the close that follows ends a write still blocked.
	told := make(chan struct{})
	go func() { defer close(told); RefuseUnserved(s.ReadWriteCloser, reason, nil) }()
	select {
	case <-told:
	case <-time.After(250 * time.Millisecond):
	}
}

// The lock is not held while writing: Close must be able to unblock a write.
func (s *answerStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	refused := s.refused
	s.answered = true
	s.mu.Unlock()
	if refused {
		return 0, net.ErrClosed
	}
	return s.ReadWriteCloser.Write(p)
}

func (s *answerStream) Close() error {
	s.refuse(s.reason())
	return s.ReadWriteCloser.Close()
}

func (s *answerStream) CloseWrite() error {
	s.refuse(s.reason())
	if half, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return nil
}

func (s *answerStream) SetWriteDeadline(deadline time.Time) error {
	if stream, ok := s.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return stream.SetWriteDeadline(deadline)
	}
	return nil
}
