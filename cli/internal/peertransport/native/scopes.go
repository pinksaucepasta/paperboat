package native

import (
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/admission"
)

type scopeKey struct{ resource, capability, direction string }

type scopeConn struct {
	net.Conn
	lease *admission.Lease
	stop  func() bool
	once  sync.Once
	err   error
}

func (c *scopeConn) Read(p []byte) (int, error) {
	if c.lease.Check() != nil {
		return 0, tailnet.ErrAdmission
	}
	return c.Conn.Read(p)
}
func (c *scopeConn) Write(p []byte) (int, error) {
	if c.lease.Check() != nil {
		return 0, tailnet.ErrAdmission
	}
	return c.Conn.Write(p)
}
func (c *scopeConn) CloseWrite() error {
	if c.lease.Check() != nil {
		return tailnet.ErrAdmission
	}
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return net.ErrClosed
}
func (c *scopeConn) Close() error {
	c.once.Do(func() { c.stop(); c.lease.Close(); c.err = c.Conn.Close() })
	return c.err
}

func (s *Session) admitScope(k scopeKey, expires time.Time) (*admission.Lease, error) {
	if !s.owner.authority.Allows(s.peerID, k.resource, k.capability, k.direction) {
		return nil, tailnet.ErrAdmission
	}
	// The session owns the watcher until disconnect; no credential or descriptor
	// deadline can cancel admitted work. Signed withdrawals still revoke it.
	lease, err := admission.Admit(s.quic.Connection.Context(), expires, nil)
	if err != nil {
		return nil, err
	}
	release, err := s.owner.authority.TrackAdmittedScope(s.peerID, k.resource, k.capability, k.direction)
	if err != nil {
		lease.Close()
		return nil, err
	}
	lease.OnDone(release)
	if err := lease.Watch(s.owner.authority.ScopeChanges, func() bool {
		return s.owner.authority.AllowsAdmitted(s.peerID, k.resource, k.capability, k.direction)
	}); err != nil {
		lease.Close()
		return nil, err
	}
	return lease, nil
}
func (s *Session) bindScope(conn net.Conn, resource, capability, direction string, expires time.Time) (net.Conn, error) {
	lease, err := s.admitScope(scopeKey{resource, capability, direction}, expires)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &scopeConn{Conn: conn, lease: lease, stop: lease.Bind(conn)}, nil
}

// One HTTP/3 association is admitted for one access resource. Revocation closes
// the association, including blocked CONNECT reads, through the shared owner.
func (s *Session) bindHTTP3Scope(resource, direction string, expires time.Time) error {
	k := scopeKey{resource, "private_access", direction}
	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	if s.http3Scope != nil {
		if *s.http3Scope != k {
			return tailnet.ErrAdmission
		}
		return nil
	}
	lease, err := s.admitScope(k, expires)
	if err != nil {
		_ = s.quic.Close()
		return err
	}
	s.http3Scope = &k
	lease.Bind(s.quic)
	return nil
}
