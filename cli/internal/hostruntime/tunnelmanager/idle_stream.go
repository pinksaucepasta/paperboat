package tunnelmanager

import (
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

// idleOriginStream applies the authoritative route's inactivity bound to each
// direction without imposing a total lifetime on a busy HTTP/TCP stream.
type idleOriginStream struct {
	io.ReadWriteCloser
	idle time.Duration
}

// upgradedStreamIdle is the inactivity bound of a WebSocket once upgraded: servers without pings go
// quiet for minutes, and active traffic resets it (Cloudflare cuts at ~100 s; we do not).
const upgradedStreamIdle = 10 * time.Minute

// extendIdle raises the bound for an upgraded stream; it runs before the bridge starts copying.
func (s *idleOriginStream) extendIdle(idle time.Duration) {
	s.idle = max(s.idle, idle)
}

func (s *idleOriginStream) Read(p []byte) (int, error) {
	if d, ok := s.ReadWriteCloser.(interface{ SetReadDeadline(time.Time) error }); ok {
		if err := d.SetReadDeadline(time.Now().Add(s.idle)); err != nil {
			return 0, err
		}
	}
	return s.ReadWriteCloser.Read(p)
}
func (s *idleOriginStream) Write(p []byte) (int, error) {
	if d, ok := s.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := d.SetWriteDeadline(time.Now().Add(s.idle)); err != nil {
			return 0, err
		}
	}
	return s.ReadWriteCloser.Write(p)
}
func (s *idleOriginStream) CloseWrite() error {
	if h, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return h.CloseWrite()
	}
	return ErrInvalidConfig
}

// boundedOriginStream applies the route's inactivity bound to an HTTP stream. Opaque TCP streams are
// left alone: the edge owns their idle limit (1 h by default, F21-37), and the route's 90 s HTTP bound
// must not cut a quiet SSH or database session.
func boundedOriginStream(stream io.ReadWriteCloser, route hoststate.TunnelConfigRoute) io.ReadWriteCloser {
	if opaqueTCP(route) {
		return stream
	}
	return &idleOriginStream{ReadWriteCloser: stream, idle: time.Duration(route.IdleTimeoutMs) * time.Millisecond}
}
