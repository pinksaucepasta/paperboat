package native

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

type carrierPacket struct {
	net.PacketConn
	remote net.Addr
}

func (p *carrierPacket) RemoteAddr() net.Addr { return p.remote }
func (*carrierPacket) MaxPayload() uint16     { return tailnet.MaxPayload }

// carrierTLS authenticates the offer's delegated key under the peer's current
// signed binding; a renewed delegation presents a newer certificate for it.
func (o *Owner) carrierTLS(peerID string, peer tailnet.NetworkBinding, offer CarrierOffer) *tls.Config {
	config := o.tls.Clone()
	config.NextProtos = []string{peerquic.ALPN}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		current, err := o.authority.Peer(peerID)
		if err != nil || current != peer || offer.verify(current, serverclock.Now()) != nil || state.NegotiatedProtocol != peerquic.ALPN || len(state.PeerCertificates) != 1 || certificateNamespace(state.PeerCertificates[0].Raw) != offer.Namespace() {
			return ErrInvalid
		}
		return nil
	}
	return config
}

// carrierSession owns transport and the network reference from here on.
func (o *Owner) carrierSession(peerID string, peer tailnet.NetworkBinding, offer CarrierOffer, transport *peerquic.Session, network *tailnet.CarrierNetwork, release func()) (*Session, error) {
	session, err := newSession(o, peerID, peer, transport)
	if err == nil {
		err = o.add(session)
	}
	if err != nil {
		transport.Close()
		release()
		return nil, err
	}
	session.carrier.OnDone(release)
	session.setCarrierNetwork(network)
	go o.watchPeerClose(session)
	return session, nil
}

// carrierAttemptLimit bounds one relay edge's preparation and dial.
const carrierAttemptLimit = 3 * time.Second

// waitRelay selects the relay for the peer off the caller, so a relay step that
// does not honour ctx (dadape direct3b sat 10 s) cannot hold the dial past its bound.
func waitRelay(ctx context.Context, selectRelay func() error) error {
	done := make(chan error, 1)
	go func() { done <- selectRelay() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ListenCarrier is a generation's mutually authenticated transport inside WG.
// The signed parent authority remains its admission and revocation owner.
func (o *Owner) ListenCarrier(ctx context.Context, socket net.PacketConn, identity *CarrierIdentity, serve func(context.Context, *Session) error, ready chan<- struct{}) error {
	if o == nil || ctx == nil || socket == nil || identity == nil || identity.tls == nil || serve == nil || ready == nil {
		return ErrInvalid
	}
	self, err := o.authority.Self()
	if err != nil || identity.Current().verify(self, serverclock.Now()) != nil {
		socket.Close()
		return ErrInvalid
	}
	config := identity.serverTLS()
	config.ClientAuth = tls.RequireAnyClientCert
	config.VerifyConnection = func(state tls.ConnectionState) error {
		current, err := o.authority.Self()
		if err != nil || identity.Current().verify(current, serverclock.Now()) != nil {
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
	defer func() {
		_ = listener.Close()
		// Owner.Close joins this worker before the carrier closes its network.
		time.Sleep(time.Until(listener.ClosingDeadline()))
	}()
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
	session, err := newSession(o, peer.EndpointID, peer, transport)
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

// dialCarrierPacket owns one bounded attempt. As with Owner.Dial, a lost pre-application handshake retries a fresh flow
// without discarding discovery/relay allocation state or replaying input.
func (o *Owner) dialCarrierPacket(ctx context.Context, network *tailnet.CarrierNetwork, peerID string, config *tls.Config, class peerquic.Class) (*peerquic.Session, error) {
	for {
		socket, err := network.Dial(ctx, peerID)
		if err != nil {
			return nil, err
		}
		transport, err := peerquic.DialPacket(ctx, &carrierPacket{PacketConn: socket, remote: socket.RemoteAddr()}, config, peerquic.DevelopmentSessionConfig(class))
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
