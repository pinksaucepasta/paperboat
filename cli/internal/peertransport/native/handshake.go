package native

import (
	"context"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
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
