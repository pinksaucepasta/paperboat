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
