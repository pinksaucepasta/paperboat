package native

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

type carrierPacket struct {
	net.PacketConn
	remote net.Addr
}

func (p *carrierPacket) RemoteAddr() net.Addr { return p.remote }
func (*carrierPacket) MaxPayload() uint16     { return tailnet.MaxPayload }

// DialCarrier inherits the old authenticated machine binding. Delegation proves
// the new key locally; it cannot create another peer or application authority.
func (s *Session) DialCarrier(ctx context.Context, offer CarrierOffer) (*Session, error) {
	if s == nil || ctx == nil || s.owner == nil || s.quic == nil || s.quic.Connection.Context().Err() != nil {
		return nil, ErrInvalid
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	peer, err := s.owner.authority.Peer(s.peerID)
	if err != nil || peer != s.binding {
		return nil, tailnet.ErrAdmission
	}
	if err := offer.verify(peer, time.Now()); err != nil {
		return nil, err
	}
	config := s.owner.tls.Clone()
	config.NextProtos = []string{peerquic.ALPN}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		current, err := s.owner.authority.Peer(s.peerID)
		if err != nil || current != peer || offer.verify(current, time.Now()) != nil || state.NegotiatedProtocol != peerquic.ALPN || len(state.PeerCertificates) != 1 || !bytes.Equal(state.PeerCertificates[0].Raw, offer.Claims.Certificate) {
			return ErrInvalid
		}
		return nil
	}
	network, release, err := s.getCarrierNetwork(ctx, offer)
	if err != nil {
		return nil, err
	}
	kept := false
	defer func() {
		if !kept {
			release()
		}
	}()
	for len(network.Endpoints()) == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-network.EndpointsChanged():
		}
	}
	if err := network.AddPeer(s.peerID, offer.Claims.DiscoPublicKey, offer.Claims.Endpoints); err != nil {
		return nil, err
	}
	// Try each common relay edge in turn, each attempt bounded, so a dead or
	// refusing relay costs about a second, not the whole offer budget. Direct
	// paths are tried in every attempt; the relay is the guarantee.
	relays := network.CommonRelays(offer.Claims.RelayNodeIDs, s.owner.authority.RegionalStatus().NodeID)
	if len(relays) == 0 {
		relays = []string{""}
	}
	// Connect every common relay at once: a dead edge costs nothing extra and
	// a slow one (dadape: >800 ms cold) is ready by the time its turn comes.
	warm := make(map[string]<-chan error, len(relays))
	for _, relay := range relays {
		if relay == "" {
			continue
		}
		done := make(chan error, 1)
		warm[relay] = done
		go func(relay string) { done <- network.WarmRelay(ctx, relay) }(relay)
	}
	var transport *peerquic.Session
	var attemptErr error
	for i, relay := range relays {
		transport, err = s.carrierAttempt(ctx, network, config, offer, peer, relay, warm[relay], i == len(relays)-1)
		if err == nil {
			break
		}
		attemptErr = errors.Join(attemptErr, err)
		diagnosticlog.TryInfo("terminal carrier move", "stage", "relay_attempt", "failed", true, "error_type", moveErrorType(err))
		if ctx.Err() != nil {
			break
		}
	}
	if transport == nil {
		return nil, attemptErr
	}

	session, err := newSession(s.owner, s.peerID, peer, transport, true)
	if err == nil {
		err = s.owner.add(session)
	}
	if err != nil {
		transport.Close()
		return nil, err
	}
	session.carrier.OnDone(release)
	kept = true
	session.setCarrierNetwork(network)
	path := network.PeerPath(s.peerID)
	s.owner.emit(Event{Kind: "carrier_moved", PeerID: s.peerID, Path: path})
	if path == "direct" {
		path = "direct-punched"
	}
	diagnosticlog.TryInfo("terminal carrier move", "stage", "connected", "path", path)
	return session, nil
}

// carrierRelayAttemptLimit bounds one relay edge's preparation so a dead relay
// is detected in under a second and the other edge is tried.
const carrierRelayAttemptLimit = 800 * time.Millisecond

// carrierAttemptLimit bounds one whole candidate exchange and dial; the last
// attempt keeps the remaining offer budget.
const carrierAttemptLimit = 3 * time.Second

func (s *Session) carrierAttempt(ctx context.Context, network *tailnet.CarrierNetwork, config *tls.Config, offer CarrierOffer, peer tailnet.NetworkBinding, relay string, warm <-chan error, last bool) (*peerquic.Session, error) {
	if !last {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, carrierAttemptLimit)
		defer cancel()
	}
	diagnosticlog.TryInfo("terminal carrier move", "stage", "relay_attempt_start", "reason", relay)
	prepare, stop := relayPrepareContext(ctx, last)
	err := waitRelay(prepare, warm, func() error { return network.PreparePeerRelay(prepare, s.peerID, relay) })
	stop()
	if err != nil {
		return nil, err
	}
	exchange := CarrierExchange{Offer: offer, DiscoPublicKey: network.DiscoPublic(), RelayNodeID: relay}
	for _, endpoint := range network.Endpoints() {
		exchange.Endpoints = append(exchange.Endpoints, endpoint.String())
	}
	current, err := s.exchangeCarrierCandidates(ctx, exchange)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(current.Claims.Certificate, offer.Claims.Certificate) || current.verify(peer, time.Now()) != nil {
		return nil, ErrInvalid
	}
	if err := network.AddPeer(s.peerID, current.Claims.DiscoPublicKey, current.Claims.Endpoints); err != nil {
		return nil, err
	}
	return s.dialCarrierPacket(ctx, network, config)
}

// relayPrepareContext cuts a relay's preparation short only when another edge
// follows it; the last edge keeps the move's budget (dadape direct2b cut both
// edges at 800 ms and fell back to a 16 s reconnect).
func relayPrepareContext(ctx context.Context, last bool) (context.Context, context.CancelFunc) {
	if last {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, carrierRelayAttemptLimit)
}

// waitRelay waits for the relay's warm connection, then selects it for the
// peer. Both steps run off the caller so a relay step that does not honour
// ctx (dadape direct3b sat 10 s) cannot hold the move past its bound.
func waitRelay(ctx context.Context, warm <-chan error, selectRelay func() error) error {
	done := make(chan error, 1)
	go func() {
		if warm != nil {
			select {
			case err := <-warm:
				if err != nil {
					done <- err
					return
				}
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		done <- selectRelay()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ListenCarrier is a mutually authenticated terminal-only transport inside WG.
// The signed parent authority remains its admission and revocation owner.
func (o *Owner) ListenCarrier(ctx context.Context, socket net.PacketConn, identity *CarrierIdentity, serve func(context.Context, *Session) error, ready chan<- struct{}) error {
	if o == nil || ctx == nil || socket == nil || identity == nil || identity.tls == nil || serve == nil || ready == nil {
		return ErrInvalid
	}
	self, err := o.authority.Self()
	if err != nil || identity.Offer.verify(self, time.Now()) != nil {
		socket.Close()
		return ErrInvalid
	}
	config := identity.tls.Clone()
	config.ClientAuth = tls.RequireAnyClientCert
	config.VerifyConnection = func(state tls.ConnectionState) error {
		current, err := o.authority.Self()
		if err != nil || identity.Offer.verify(current, time.Now()) != nil {
			return ErrInvalid
		}
		peer, err := o.carrierPeer(state)
		if err != nil {
			return err
		}
		return boundTLS(o.tls, peer, true).VerifyConnection(state)
	}
	listener, err := peerquic.ListenPacket(&carrierPacket{PacketConn: socket}, config, peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive))
	if err != nil {
		return err
	}
	defer listener.Close()
	close(ready)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	ownerStop := context.AfterFunc(o.ctx, func() { _ = listener.Close() })
	defer ownerStop()
	handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
	transport, err := listener.Accept(handshake)
	cancel()
	if err != nil {
		return err
	}
	peer, err := o.carrierPeer(transport.Connection.ConnectionState().TLS)
	if err != nil {
		transport.Close()
		return err
	}
	session, err := newSession(o, peer.EndpointID, peer, transport, true)
	if err != nil {
		transport.Close()
		return err
	}
	if err := o.add(session); err != nil {
		session.Close()
		return err
	}
	defer session.Close()
	return serve(ctx, session)

}

func (o *Owner) carrierPeer(state tls.ConnectionState) (tailnet.NetworkBinding, error) {
	if len(state.PeerCertificates) != 1 || state.NegotiatedProtocol != peerquic.ALPN {
		return tailnet.NetworkBinding{}, ErrInvalid
	}
	public, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return tailnet.NetworkBinding{}, ErrInvalid
	}
	peer, err := o.authority.PeerForQUICKey(public)
	return peer, err
}

// Preparation owns a single bounded attempt while the old terminal serves.
// As with Owner.Dial, a lost pre-application handshake retries a fresh flow
// without discarding discovery/relay allocation state or replaying input.
func (s *Session) dialCarrierPacket(ctx context.Context, network *tailnet.CarrierNetwork, config *tls.Config) (*peerquic.Session, error) {
	for {
		socket, err := network.Dial(ctx, s.peerID)
		if err != nil {
			return nil, err
		}
		transport, err := peerquic.DialPacket(ctx, &carrierPacket{PacketConn: socket, remote: socket.RemoteAddr()}, config, peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive))
		if err == nil {
			return transport, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		failure := classifyPeerOperationFailure(ctx, "peer_connect", "native_private_failed", err)
		if !retryablePeerDial(failure) {
			return nil, failure
		}
	}
}
