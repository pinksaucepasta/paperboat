package native

import (
	"context"
	"errors"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
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
	if s.closing.Load() || !errors.As(context.Cause(s.quic.Connection.Context()), &closed) || !closed.Remote {
		return
	}
	if closed.ErrorCode == peerquic.RestartingCode {
		// The retiring generation's route is stale and the signed configuration
		// names the current one: the reconnect dials it, not the old route.
		o.mu.Lock()
		route := o.routes[s.peerID]
		o.mu.Unlock()
		if route != nil {
			o.dropRoute(s.peerID, route)
		}
		o.authority.RequestUrgentRefresh()
	}
	o.mu.Lock()
	carrier := s.carrierNetwork != nil
	o.mu.Unlock()
	if !carrier && (closed.ErrorCode == 0 || closed.ErrorCode == peerquic.RestartingCode) {
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
	if !closed || !o.authority.RetireIdleClient(client) {
		return client, nil
	}
	return o.authority.Client(descriptor, peerID)
}
