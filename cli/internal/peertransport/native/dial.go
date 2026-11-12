package native

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
)

func retryablePeerDial(err error) bool {
	var failure *peerOperationFailure
	var idle *quic.IdleTimeoutError
	var transport *quic.TransportError
	var application *quic.ApplicationError
	// QUIC protocol/TLS and application rejections also unwrap to net.ErrClosed;
	// that sentinel alone must not turn a rejection into transport recovery.
	if errors.As(err, &transport) || errors.As(err, &application) {
		return false
	}
	return errors.As(err, &failure) && failure.retryable &&
		(errors.Is(err, net.ErrClosed) || errors.As(err, &idle))
}

func (o *Owner) failedPeerDial(ctx context.Context, peerID string, client *tailnet.UDPClient, err error) error {
	failure := o.dialFailure(ctx, peerID, "peer_connect", "native_private_failed", err)
	if ctx.Err() == nil && retryablePeerDial(failure) && !o.keepingRelayPath(peerID) {
		if !o.authority.RetireIdleClient(client) {
			// Other flows still own the client, so its stale WireGuard state cannot be dropped with it. Start a fresh
			// initiation for the next attempt: WireGuard repeats an unanswered one only after its 5 s RekeyTimeout.
			o.authority.ResetPeerPath(client)
		}
	}
	return failure
}

// A fresh client's first WireGuard initiation leaves before its peer relay path exists (allocation, bind and
// ping take a round trip through the relay), and wireguard-go repeats it only after RekeyTimeout, 5 s plus up to
// 1/3 s jitter (wireguard-go device/timers.go). Retiring the client after each one-second attempt discarded
// the found path, so under load a peer reachable only through a peer relay was never reached. A failed attempt
// that found the path keeps the client through that retry and one more attempt. A dead peer finds no new
// path: its stale state is still retired at once.
const relayPathKeep = 7 * time.Second

func (o *Owner) keepRelayPath(peerID string, found time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.relayPaths == nil {
		o.relayPaths = make(map[string]time.Time)
	}
	o.relayPaths[peerID] = found.Add(relayPathKeep)
}

func (o *Owner) keepingRelayPath(peerID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	until, ok := o.relayPaths[peerID]
	if ok && !time.Now().Before(until) {
		delete(o.relayPaths, peerID)
		return false
	}
	return ok
}

// Dial obtains a new QUIC association before any application request is sent.
// Recovering a lost handshake cannot replay exec, terminal input or transfer bytes.
func (o *Owner) Dial(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	// Non-admitted callers also need a finite setup budget. Admitted terminal
	// setup supplies its own credential deadline through the local API.
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	// The admission/caller context bounds setup; silent handshakes each expire
	// after one second. Do not return an intermediate 503 while recovery runs.
	dialCtx, cancel := relayWithheldDeadline(ctx, o.authority)
	defer cancel()
	for {
		session, err := o.dialAttempt(dialCtx, descriptor, peerID, class)
		if err == nil {
			return session, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if dialCtx.Err() != nil || !retryablePeerDial(err) {
			return nil, relayQuotaFailure(o.authority, err)
		}
		descriptor, err = o.authority.Descriptor(peerID)
		if err != nil {
			return nil, err
		}
	}
}

// sharedDial is one in-flight or finished association of DialShared.
type sharedDial struct {
	done chan struct{}
	base *Session
	err  error
}

type sharedKey struct {
	peerID string
	class  peerquic.Class
}

// DialShared is Dial with one QUIC association per peer and class: every caller gets a handle on the same association and
// opens its terminals and commands as streams on it, as Tailscale carries every connection to a peer over one tunnel. A
// dial per terminal pinned one of the client's 64 UDP flows each (tailnet.MaxFlows) and stopped a client at ~40 terminals
// (F21-164). The association closes when its last handle does, or with the peer; a closed one is dialed again.
func (o *Owner) DialShared(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	key := sharedKey{peerID, class}
	for {
		o.mu.Lock()
		if o.shared == nil {
			o.shared = make(map[sharedKey]*sharedDial)
		}
		entry := o.shared[key]
		if entry != nil && entry.base != nil {
			if handle, ok := entry.base.share(); ok {
				o.mu.Unlock()
				return handle, nil
			}
			delete(o.shared, key)
			entry = nil
		}
		if entry == nil {
			entry = &sharedDial{done: make(chan struct{})}
			o.shared[key] = entry
			o.mu.Unlock()
			base, err := o.Dial(ctx, descriptor, peerID, class)
			o.mu.Lock()
			entry.base, entry.err = base, err
			if err != nil {
				delete(o.shared, key)
			}
			close(entry.done)
			o.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if handle, ok := base.share(); ok {
				return handle, nil
			}
			continue // closed before its first handle: dial again
		}
		o.mu.Unlock()
		select {
		case <-entry.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// A leader's own cancellation says nothing about the peer: retry. Any other failure is the peer's answer.
		if entry.err != nil && !errors.Is(entry.err, context.Canceled) && !errors.Is(entry.err, context.DeadlineExceeded) {
			return nil, entry.err
		}
	}
}

// OpenSessions is how many QUIC associations the owner holds (each pins one UDP flow).
func (o *Owner) OpenSessions() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sessions)
}
