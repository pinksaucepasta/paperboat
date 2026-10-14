package native

import (
	"net"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

type scopeKey struct{ resource, capability, direction string }

type scopeConn struct {
	net.Conn
	session *Session
	key     scopeKey
	once    sync.Once
	err     error
}

func (c *scopeConn) allowed() bool {
	return c.session.owner.authority.Allows(c.session.peerID, c.key.resource, c.key.capability, c.key.direction)
}
func (c *scopeConn) Read(p []byte) (int, error) {
	if !c.allowed() {
		c.Close()
		return 0, tailnet.ErrAdmission
	}
	return c.Conn.Read(p)
}
func (c *scopeConn) Write(p []byte) (int, error) {
	if !c.allowed() {
		c.Close()
		return 0, tailnet.ErrAdmission
	}
	return c.Conn.Write(p)
}
func (c *scopeConn) CloseWrite() error {
	if !c.allowed() {
		c.Close()
		return tailnet.ErrAdmission
	}
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return net.ErrClosed
}
func (c *scopeConn) Close() error {
	c.once.Do(func() {
		c.session.scopeMu.Lock()
		delete(c.session.scopeConns, c)
		c.session.scopeMu.Unlock()
		c.err = c.Conn.Close()
	})
	return c.err
}
func (s *Session) bindScope(conn net.Conn, resource, capability, direction string) (net.Conn, error) {
	c := &scopeConn{Conn: conn, session: s, key: scopeKey{resource, capability, direction}}
	s.scopeMu.Lock()
	s.scopeConns[c] = struct{}{}
	s.scopeMu.Unlock()
	if !c.allowed() {
		c.Close()
		return nil, tailnet.ErrAdmission
	}
	return c, nil
}

// A private HTTP/3 connection is constructed for one access grant. Keeping the
// grant on the connection fences blocked CONNECT reads as well as new requests.
func (s *Session) bindHTTP3Scope(resource, direction string) error {
	k := scopeKey{resource, "private_access", direction}
	s.scopeMu.Lock()
	if s.http3Scope != nil && *s.http3Scope != k {
		s.scopeMu.Unlock()
		return tailnet.ErrAdmission
	}
	s.http3Scope = &k
	s.scopeMu.Unlock()
	if !s.owner.authority.Allows(s.peerID, k.resource, k.capability, k.direction) {
		s.quic.Close()
		return tailnet.ErrAdmission
	}
	return nil
}
func (s *Session) watchScopes() {
	defer close(s.scopeDone)
	for {
		changes := s.owner.authority.ScopeChanges()
		s.scopeMu.Lock()
		conns := make([]*scopeConn, 0, len(s.scopeConns))
		for c := range s.scopeConns {
			conns = append(conns, c)
		}
		h3 := s.http3Scope
		s.scopeMu.Unlock()
		for _, c := range conns {
			if !c.allowed() {
				c.Close()
			}
		}
		if h3 != nil && !s.owner.authority.Allows(s.peerID, h3.resource, h3.capability, h3.direction) {
			s.quic.Close()
			return
		}
		select {
		case <-changes:
		case <-s.quic.Connection.Context().Done():
			return
		}
	}
}
