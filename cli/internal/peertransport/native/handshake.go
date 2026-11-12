package native

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
)

type peerHandshake struct {
	token  chan struct{}
	users  int
	warm   chan struct{} // closed once a dial to this peer got through: its WireGuard lease is up, so setups no longer take turns
	warmed bool
}

// Pending QUIC handshakes must not keep each other's stale WireGuard lease alive. Serialize setup per peer until one
// dial has got through (the lease is then up and later setups are independent); admitted sessions and other peers stay
// free. A failed dial makes the peer cold again. Taking turns after the first success cost one round trip per exec:
// 32 concurrent execs over a 260 ms relay finished up to 8 s late (F21-166).
func (o *Owner) dialAttempt(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	end := diagnostics.ReconnectStep(ctx, "handshake_lease")
	release, err := o.acquireHandshake(ctx, peerID)
	end(err)
	if err != nil {
		return nil, err
	}
	defer release()
	session, err := o.dialOnce(ctx, descriptor, peerID, class)
	o.markHandshakeWarm(peerID, err == nil)
	return session, err
}

// markHandshakeWarm records whether the last dial to peerID got through.
func (o *Owner) markHandshakeWarm(peerID string, through bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	slot := o.handshakes[peerID]
	if slot == nil {
		return
	}
	switch {
	case through && !slot.warmed:
		slot.warmed = true
		close(slot.warm)
	case !through && slot.warmed:
		slot.warmed, slot.warm = false, make(chan struct{})
	}
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
		slot = &peerHandshake{token: make(chan struct{}, 1), warm: make(chan struct{})}
		o.handshakes[peerID] = slot
	}
	slot.users++
	warm := slot.warm
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
	case <-warm:
		return func() { release(false) }, nil
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
		o.restart.markClosed(s.peerID, time.Now())
		o.mu.Unlock()
	}
}

// peerRestartHandshake is the floor of the handshake timeout of dials to a peer that just closed cleanly: the restarted
// peer may not be answering yet, and the dial that follows a lost first flight completes in milliseconds (pbvm4: 1.0 s
// lost, then 28 ms). The wait grows with the peer's own measured handshake (restartHandshake): a fixed 300 ms can never
// complete on a relay or long-haul path.
// Prior art for a fixed short reconnect cadence: EternalTerminal retries every second (~/ref/EternalTerminal/src/base/
// ClientConnection.cpp:276); Tailscale's defaults (heartbeat 3 s, ping timeout 5 s, ~/ref/tailscale/wgengine/magicsock/
// magicsock.go:4082-4110) are slower, which is why only a dial that knows the peer just restarted shortens its wait.
const peerRestartHandshake = 300 * time.Millisecond

// restartHandshake is the handshake timeout for dials to a restarted peer whose last handshake took `took`: three times
// that, so a lost first flight and its retransmit still fit, never below peerRestartHandshake. A path that needs a third
// of the normal second or more gets the normal timeout (0), as a short cap would only make every dial fail.
func restartHandshake(took time.Duration) time.Duration {
	wait := max(peerRestartHandshake, 3*took)
	if wait >= time.Second {
		return 0
	}
	return wait
}

// handshakeWait is the handshake timeout of a dial to peerID: the restart wait while its restart window is open, and
// always for a peer on this machine, which answers in milliseconds. An updated daemon is a new process and holds no
// restart mark for the peer whose carrier the same update replaced, so a local peer cannot depend on one. 0 is the
// normal timeout.
func (o *Owner) handshakeWait(peerID string) time.Duration {
	local := o.authority.PeerIsLocal(peerID)
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.restart.wait(peerID, local, time.Now())
}

// freshClientAfterPeerClose consumes the peer's restart mark on whichever dial comes first (the WireGuard state it
// retires or resets belongs to the peer, not to the dial) and opens the restart window: every dial to the peer, of any
// class, uses the short handshake wait until one succeeds (restartTracker.wait).
func (o *Owner) freshClientAfterPeerClose(descriptor mesh.Addr, peerID string, client *tailnet.UDPClient) (*tailnet.UDPClient, error) {
	o.mu.Lock()
	closed := o.restart.consume(peerID, time.Now())
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
	// state so the next flow handshakes at once; the client and its sessions stay. Another session that still looks
	// alive does not mean the peer did not restart: the same restart closed it, and its close is a few milliseconds
	// behind (pbvm4: 2 of 5 updates paused 16 s when this was skipped).
	o.authority.ResetPeerPath(client)
	return client, nil
}
