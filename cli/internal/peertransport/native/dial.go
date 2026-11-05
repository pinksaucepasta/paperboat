package native

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
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
		o.authority.RetireIdleClient(client)
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
	// Private HTTP/3 negotiates its own ALPN, which a generation does not serve.
	if class != peerquic.ClassPreview {
		o.mu.Lock()
		route := o.routes[peerID]
		o.mu.Unlock()
		if session := o.dialRoute(dialCtx, peerID, nil, class); session != nil {
			return session, nil
		}
		if route != nil && dialCtx.Err() == nil {
			// The route's generation did not answer: do not try it again from the configuration.
			if raw, err := route.offer.MarshalBinary(); err == nil {
				o.authority.DeclineCarrierOffer(peerID, raw)
			}
		}
		if session := o.dialOffered(dialCtx, peerID, class); session != nil {
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

// dialOffered opens a new connection on the carrier generation the peer's signed configuration names, so a
// client that was not connected when the machine moved needs neither the old carrier nor a candidate
// exchange. The client's meow over a common relay admits it on the generation, as a fresh peer is admitted
// on the primary (mesh onMeow; Tailscale learns a peer's DERP route from its packets the same way,
// ~/ref/tailscale/wgengine/magicsock/derp.go addDerpPeerRoute). Without a common relay the generation
// cannot learn this client's discovery key, so the caller dials the primary as before. Success sets the
// route, so later dials skip the configuration.
func (o *Owner) dialOffered(ctx context.Context, peerID string, class peerquic.Class) *Session {
	raw := o.authority.PeerCarrierOffer(peerID)
	if raw == nil {
		return nil
	}
	peer, err := o.authority.Peer(peerID)
	if err != nil {
		return nil
	}
	offer, err := ParseCarrierOffer(raw)
	if err != nil || !offer.Claims.AllApplications || offer.verify(peer, time.Now()) != nil {
		o.authority.DeclineCarrierOffer(peerID, raw)
		return nil
	}
	network, release, err := o.getCarrierNetwork(ctx, offer)
	if err != nil {
		return nil
	}
	transport, err := o.connectOffered(ctx, network, peerID, peer, offer, class)
	if err != nil {
		release()
		if ctx.Err() == nil {
			o.authority.DeclineCarrierOffer(peerID, raw)
		}
		diagnosticlog.TryInfo("carrier route", "stage", "offered_dial", "failed", true, "error_type", moveErrorType(err))
		return nil
	}
	session, err := o.carrierSession(peerID, peer, offer, transport, network, release)
	if err != nil {
		return nil
	}
	o.setRoute(ctx, peerID, offer)
	diagnosticlog.TryInfo("carrier route", "stage", "offered_dial", "path", network.PeerPath(peerID))
	return session
}

// connectOffered tries each common relay once, each attempt bounded by carrierAttemptLimit.
func (o *Owner) connectOffered(ctx context.Context, network *tailnet.CarrierNetwork, peerID string, peer tailnet.NetworkBinding, offer CarrierOffer, class peerquic.Class) (*peerquic.Session, error) {
	relays := network.CommonRelays(offer.Claims.RelayNodeIDs, o.authority.RegionalStatus().NodeID)
	if len(relays) == 0 {
		return nil, ErrInvalid
	}
	if err := network.AddPeer(peerID, offer.Claims.DiscoPublicKey, offer.Claims.Endpoints); err != nil {
		return nil, err
	}
	var errs error
	for _, relay := range relays {
		attempt, cancel := context.WithTimeout(ctx, carrierAttemptLimit)
		err := waitRelay(attempt, nil, func() error { return network.PreparePeerRelay(attempt, peerID, relay) })
		var transport *peerquic.Session
		if err == nil {
			transport, err = o.dialCarrierPacket(attempt, network, peerID, o.carrierTLS(peerID, peer, offer), class)
		}
		cancel()
		if err == nil {
			return transport, nil
		}
		if errs = errors.Join(errs, err); ctx.Err() != nil {
			break
		}
	}
	return nil, errs
}
