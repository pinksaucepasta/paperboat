package managedssh

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
)

// Session audit (F5-12). Managed SSH has no server-side admission, so the host that serves a session says who
// started it, what it moved and how it ended; Host.Serve is the one owner, whichever path the stream took.

// Peer is who an SSH stream belongs to. Offline is a session the host admitted from its own signed scopes while
// Paperboat was unreachable.
type Peer struct {
	AccountID   string
	EndpointID  string
	OperationID string
	Offline     bool
}

type peerKey struct{}

// WithPeer names the peer of the streams Serve is given with ctx. A stream served without one is not audited.
func WithPeer(ctx context.Context, peer Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, peer)
}

func peerFrom(ctx context.Context) (Peer, bool) {
	peer, ok := ctx.Value(peerKey{}).(Peer)
	return peer, ok && peer.AccountID != "" && peer.EndpointID != ""
}

const (
	SessionStarted = "started"
	SessionRefused = "refused"
	SessionEnded   = "ended"

	// Why a session was refused.
	RefusedBusy     = "busy"
	RefusedNoAccess = "no_access"
	RefusedNotSetUp = "not_set_up"

	// How a session ended.
	EndedClosed  = "closed"
	EndedRevoked = "revoked"
	EndedFailed  = "failed"
)

// SessionEvent is one audit record. SessionID names the session: a start carries it and its end repeats it.
type SessionEvent struct {
	Kind       string
	Peer       Peer
	Generation uint64
	SessionID  string
	Reason     string // refused
	Outcome    string // ended
	BytesIn    int64  // ended: bytes the client sent to sshd
	BytesOut   int64  // ended: bytes sshd sent to the client
}

// SessionAudit records a session event. An error from a start refuses an offline session (its audit must be on
// disk before it begins); for an online session the host serves it anyway and the audit is best effort.
type SessionAudit func(SessionEvent) error

// ErrSSHAuditUnavailable refuses an offline session whose start could not be recorded.
var ErrSSHAuditUnavailable = errors.New("managed SSH session could not be recorded for audit; it needs Paperboat online")

// Refused tells the client why its stream is refused and audits it.
func (h *Host) Refused(ctx context.Context, stream io.Writer, generation uint64, reason string) {
	writeRefusal(stream, reason)
	if h == nil || h.config.Audit == nil {
		return
	}
	if peer, ok := peerFrom(ctx); ok {
		_ = h.config.Audit(SessionEvent{Kind: SessionRefused, Peer: peer, Generation: generation, Reason: reason})
	}
}

// begin audits the start of a served session. It returns the session ID and the audit to end it with.
func (h *Host) begin(ctx context.Context, generation uint64) (sessionID string, peer Peer, audited bool, err error) {
	peer, audited = peerFrom(ctx)
	if h.config.Audit == nil || !audited {
		return "", Peer{}, false, nil
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", Peer{}, false, err
	}
	sessionID = id.String()
	if err := h.config.Audit(SessionEvent{Kind: SessionStarted, Peer: peer, Generation: generation, SessionID: sessionID}); err != nil && peer.Offline {
		return "", Peer{}, false, errors.Join(ErrSSHAuditUnavailable, err)
	}
	return sessionID, peer, true, nil
}

func (h *Host) end(peer Peer, generation uint64, sessionID, outcome string, result BridgeResult) {
	_ = h.config.Audit(SessionEvent{Kind: SessionEnded, Peer: peer, Generation: generation, SessionID: sessionID, Outcome: outcome, BytesIn: result.ToSSHD, BytesOut: result.FromSSHD})
}
