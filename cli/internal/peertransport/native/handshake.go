package native

import (
	"context"
	"errors"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
)

type peerHandshake struct {
	token chan struct{}
	users int
}

// Pending QUIC handshakes must not keep each other's stale WireGuard lease
// alive. Serialize setup per peer; admitted sessions and other peers stay free.
func (o *Owner) dialAttempt(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	end := diagnostics.ReconnectStep(ctx, "handshake_lease")
	release, err := o.acquireHandshake(ctx, peerID)
	end(err)
	if err != nil {
		return nil, err
	}
	defer release()
	return o.dialOnce(ctx, descriptor, peerID, class)
}

func (o *Owner) acquireHandshake(ctx context.Context, peerID string) (func(), error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil, net.ErrClosed
	}
	if o.handshakes == nil {
		o.handshakes = make(map[string]*peerHandshake)
	}
	slot := o.handshakes[peerID]
	if slot == nil {
		slot = &peerHandshake{token: make(chan struct{}, 1)}
		o.handshakes[peerID] = slot
	}
	slot.users++
	o.mu.Unlock()
	release := func(held bool) {
		if held {
			<-slot.token
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		slot.users--
		if slot.users == 0 {
			delete(o.handshakes, peerID)
		}
	}
	select {
	case slot.token <- struct{}{}:
		return func() { release(true) }, nil
	case <-ctx.Done():
		release(false)
		return nil, ctx.Err()
	case <-o.ctx.Done():
		release(false)
		return nil, net.ErrClosed
	}
}

// A peer that closes a dialed session on purpose (its carrier is stopping for an update) takes its
// WireGuard keys with it, so the next dial's first flight on the cached lease would go out on dead keys
// and be lost until the 1 s handshake idle timeout retires the lease (pbvm4 run8: 1001 ms, then 6 ms).
// Retire the idle lease up front instead, as that timeout would.
func (o *Owner) watchPeerClose(s *Session) {
	<-s.quic.Connection.Context().Done()
	var closed *quic.ApplicationError
	if !s.closing.Load() && errors.As(context.Cause(s.quic.Connection.Context()), &closed) && closed.Remote && closed.ErrorCode == 0 {
		o.mu.Lock()
		if o.restarted == nil {
			o.restarted = make(map[string]bool)
		}
		o.restarted[s.peerID] = true
		o.mu.Unlock()
	}
}

func (o *Owner) freshClientAfterPeerClose(descriptor mesh.Addr, peerID string, client *tailnet.UDPClient) (*tailnet.UDPClient, error) {
	o.mu.Lock()
	closed := o.restarted[peerID]
	delete(o.restarted, peerID)
	o.mu.Unlock()
	if !closed {
		return client, nil
	}
	if o.authority.RetireIdleClient(client) {
		return o.authority.Client(descriptor, peerID)
	}
	// Other sessions to the peer still own the client (an exec loop's, not released yet), so it cannot be retired, but
	// the peer restarted and took its WireGuard keys with it: data on the old keys is dropped until WireGuard's own 15 s
	// no-reply rekey (pbvm4 update: 7 handshake failures at 1 s, then success at +12 s). Reset the peer's WireGuard
	// state so the next flow handshakes at once; the client and its sessions stay. A session to the peer that is still
	// alive means the close was not a restart.
	live := o.peerLiveSessions(peerID)
	if live == 0 {
		o.authority.ResetPeerPath(client)
	}
	diagnosticlog.TryInfo("native packet lifecycle", "stage", "peer_close_handled", "live_sessions", live)
	return client, nil
}

// peerLiveSessions counts the sessions to peerID still running: ones the peer did not close.
func (o *Owner) peerLiveSessions(peerID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	live := 0
	for session := range o.sessions {
		if session.peerID == peerID && session.quic.Connection.Context().Err() == nil {
			live++
		}
	}
	return live
}
