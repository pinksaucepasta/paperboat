package native

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// ResumeConn is a terminal stream admitted to move an existing attachment without a fresh
// credential. The host application must still prove the attachment's resume secret, held only by
// the PTY owner, before it acts on any request.
type ResumeConn struct {
	net.Conn
	peer     tailnet.NetworkBinding
	header   streamauth.Header
	standing bool
}

// ResumePeer names the peer proven by this native session, the terminal session and access
// session it asks to resume, and whether a standing host rule (not that access session's scope)
// admitted it.
func (c *ResumeConn) ResumePeer() (accountID, endpointID, sessionID, resource string, standing bool) {
	return c.peer.AccountID, c.peer.EndpointID, c.header.OperationID, c.header.Resume, c.standing
}

// CloseWrite keeps half-close working through the wrapper.
func (c *ResumeConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

// acceptResume admits a terminal move from the host's current authority only: the peer must still
// be this session's exact network identity and hold terminal access here now, through the named
// access session's scope or a standing host rule; a revoked machine, account or grant has neither.
// Like EternalTerminal's reconnect (~/ref/EternalTerminal/src/base/ServerClientConnection.cpp:93),
// the attachment's passkey is checked by the terminal owner, not by the transport.
func (s *Session) acceptResume(ctx context.Context, connection net.Conn, header streamauth.Header) (net.Conn, error) {
	if peer, err := s.owner.authority.Peer(s.peerID); err != nil || peer != s.binding {
		return nil, errors.Join(tailnet.ErrAdmission, err)
	}
	resource, err := s.authorizeIncoming(ctx, header, "terminal", func(context.Context, streamauth.Header) (string, error) {
		return header.Resume, nil
	})
	if err != nil {
		return nil, err
	}
	scoped, err := s.bindScope(newLimitedConn(connection, header.MaximumBytes), resource, "terminal", "accept", time.Unix(header.DeadlineUnix, 0))
	if err != nil {
		return nil, err
	}
	s.terminalUsed.Store(true)
	if err := acknowledgeAdmission(connection); err != nil {
		return nil, errors.Join(ErrStreamRejected, err, scoped.Close())
	}
	return &ResumeConn{Conn: s.MeterIncoming(scoped, header, resource, false), peer: s.binding, header: header, standing: resource != header.Resume}, nil
}
