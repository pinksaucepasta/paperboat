package native

import (
	"errors"
	"fmt"
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

// leaseEnd is why the lease ended, as the connection's error: revoked access is an admission failure; the client's
// connection ending (the lease lives under the QUIC connection) or the lease being closed means the client is already
// gone, which is a closed stream (net.ErrClosed), not a refusal. Read and Write once reported every lease end as an
// admission error: a client that left as sshd ended audited its finished macOS session as failed (F5-12).
func (c *scopeConn) leaseEnd() error {
	cause := c.lease.Check()
	switch {
	case cause == nil:
		return nil
	case errors.Is(cause, admission.ErrRevoked):
		return tailnet.ErrAdmission
	}
	return fmt.Errorf("the client's connection ended: %w", net.ErrClosed)
}

func (c *scopeConn) Read(p []byte) (int, error) {
	if err := c.leaseEnd(); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *scopeConn) Write(p []byte) (int, error) {
	if err := c.leaseEnd(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// CloseWrite sends the FIN; a lease that already ended makes it moot (see leaseEnd). Measured on a fresh Windows
// carrier (F5-12): the late Windows sshd end let the client leave first and this returned a plain admission error.
func (c *scopeConn) CloseWrite() error {
	if err := c.leaseEnd(); err != nil {
		return err
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
	// A dial on the personal mesh pair (dialStandingResource) is held by that pair, which the carrier already tracks.
	personal := k.direction == "dial" && k.resource != "" && k.resource == s.owner.authority.PersonalScope(s.peerID)
	if !personal && !s.owner.authority.Allows(s.peerID, k.resource, k.capability, k.direction) {
		return nil, tailnet.ErrAdmission
	}
	// The session owns the watcher until disconnect; no credential or descriptor
	// deadline can cancel admitted work. Signed withdrawals still revoke it.
	lease, err := admission.Admit(s.quic.Connection.Context(), expires, nil)
	if err != nil {
		return nil, err
	}
	if !personal {
		release, err := s.owner.authority.TrackAdmittedScope(s.peerID, k.resource, k.capability, k.direction)
		if err != nil {
			lease.Close()
			return nil, err
		}
		lease.OnDone(release)
	}
	if err := lease.Watch(s.owner.authority.ScopeChanges, func() bool {
		if personal {
			return s.owner.authority.PersonalScope(s.peerID) == k.resource
		}
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
