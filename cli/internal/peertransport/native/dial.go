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
	if ctx.Err() == nil && retryablePeerDial(failure) {
		o.authority.RetireIdleClient(client)
	}
	return failure
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
	// Private HTTP/3 negotiates its own ALPN, which a generation does not serve.
	if class != peerquic.ClassPreview {
		if session := o.dialRoute(dialCtx, peerID, nil, class); session != nil {
			return session, nil
		}
	}
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
