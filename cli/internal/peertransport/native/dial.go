package native

import (
	"context"
	"errors"
	"net"

	"github.com/pinksaucepasta/paperboat-relay/reattach"
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
	if ctx.Err() == nil && retryablePeerDial(failure) {
		o.authority.RetireIdleClient(client)
	}
	return failure
}

// Dial obtains a new QUIC association before any application request is sent.
// Recovering a lost handshake cannot replay exec, terminal input or transfer bytes.
func (o *Owner) Dial(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	var session *Session
	attempts := 0
	// Without a relay a peer that is not reachable directly cannot be dialed at all. Leave time
	// before the caller's deadline to answer with the plan-limit refusal instead of a timeout.
	dialCtx, cancel := relayWithheldDeadline(ctx, o.authority)
	defer cancel()
	recovery := reattach.New(reattach.Policy{Attempts: 2, Retryable: retryablePeerDial})
	err := recovery.Run(dialCtx, func(ctx context.Context) error {
		if attempts > 0 {
			fresh, err := o.authority.Descriptor(peerID)
			if err != nil {
				return err
			}
			descriptor = fresh
		}
		attempts++
		var err error
		session, err = o.dialOnce(ctx, descriptor, peerID, class)
		return err
	})
	if err != nil && ctx.Err() == nil {
		err = relayQuotaFailure(o.authority, err)
	}
	return session, err
}
