package native

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// OfflineAdmission is one request to start a session without Paperboat (F21-11): the peer is the
// one the native session proved, Allowed is the host's signed scope decision for it.
type OfflineAdmission struct {
	Peer       tailnet.NetworkBinding
	Header     streamauth.Header
	Capability string
	Allowed    bool
}

// ErrOfflineRefused is returned to the host's own observers; the client sees a closed stream.
var ErrOfflineRefused = errors.New("offline session refused; it needs Paperboat online")

// OfflineConn is an admitted offline stream. The host application authorizes its requests from
// OfflinePeer instead of a server-signed credential.
type OfflineConn struct {
	net.Conn
	peer   tailnet.NetworkBinding
	header streamauth.Header
}

// OfflinePeer names the account and endpoint the signed scope admitted and the one session the
// stream was opened for.
func (c *OfflineConn) OfflinePeer() (accountID, endpointID, consumer, operationID string, deadline time.Time) {
	return c.peer.AccountID, c.peer.EndpointID, c.header.Consumer, c.header.OperationID, time.Unix(c.header.DeadlineUnix, 0)
}

// CloseWrite keeps half-close working through the wrapper.
func (c *OfflineConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

// acceptOffline decides an offline header with the same scope check as an online one, with the
// host itself as the resource: Allows(peer, self, capability, "accept"). The host's admitOffline
// records the decision first (audit fails closed) and may refuse, e.g. when its backlog is full.
func (s *Session) acceptOffline(ctx context.Context, connection net.Conn, header streamauth.Header, capability string) (net.Conn, error) {
	if s.owner.admitOffline == nil {
		return nil, ErrOfflineRefused
	}
	self, err := s.owner.authority.Self()
	if err != nil {
		return nil, errors.Join(tailnet.ErrAdmission, err)
	}
	allowed := s.owner.authority.Allows(s.peerID, self.MachineID, capability, "accept")
	if !allowed && header.Consumer == "ssh" {
		streamauth.WriteSSHRefusal(connection, "no_access") // managedssh.RefusedNoAccess: pb says so, not OpenSSH
	}
	if err := s.owner.admitOffline(ctx, OfflineAdmission{Peer: s.binding, Header: header, Capability: capability, Allowed: allowed}); err != nil {
		return nil, errors.Join(ErrOfflineRefused, err)
	}
	if !allowed {
		return nil, tailnet.ErrAdmission
	}
	scoped, err := s.bindScope(newLimitedConn(connection, header.MaximumBytes), self.MachineID, capability, "accept", time.Unix(header.DeadlineUnix, 0))
	if err != nil {
		return nil, err
	}
	return &OfflineConn{Conn: s.MeterIncoming(scoped, header, self.MachineID, false), peer: s.binding, header: header}, nil
}

// OpenOffline asks the host to start a session from its own signed scopes. The client holds no
// scope for it; the host decides, and refuses by closing the stream.
func (s *Session) OpenOffline(ctx context.Context, header streamauth.Header) (net.Conn, error) {
	if !header.Offline {
		return nil, streamauth.ErrInvalid
	}
	encoded, err := header.MarshalBinary()
	if err != nil {
		return nil, err
	}
	connection, err := s.Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeHeader(connection, encoded); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return newLimitedConn(connection, header.MaximumBytes), nil
}
