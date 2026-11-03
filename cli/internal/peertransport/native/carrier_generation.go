package native

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"net"
	"net/netip"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
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
	var last error
	for _, endpoint := range offer.Claims.Endpoints {
		remote := netip.MustParseAddrPort(endpoint) // verified above
		network := "udp6"
		if remote.Addr().Is4() {
			network = "udp4"
		}
		socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, ":0")
		if err != nil {
			return nil, err
		}
		transport, err := peerquic.DialPacket(ctx, &carrierPacket{PacketConn: socket, remote: net.UDPAddrFromAddrPort(remote)}, config, peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive))
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		session, err := newSession(s.owner, s.peerID, peer, transport, true)
		if err == nil {
			err = s.owner.add(session)
		}
		if err != nil {
			transport.Close()
			return nil, err
		}
		return session, nil
	}
	return nil, last
}

// ListenCarrier is a direct, mutually authenticated terminal-only transport.
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
	for {
		transport, err := listener.Accept(ctx)
		if err != nil {
			return err
		}
		peer, err := o.carrierPeer(transport.Connection.ConnectionState().TLS)
		if err != nil {
			transport.Close()
			continue
		}
		session, err := newSession(o, peer.EndpointID, peer, transport, true)
		if err != nil {
			transport.Close()
			continue
		}
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			session.Close()
			return net.ErrClosed
		}
		o.sessions[session] = struct{}{}
		o.workers.Add(1)
		o.mu.Unlock()
		go func() {
			defer o.workers.Done()
			defer session.Close()
			if err := serve(ctx, session); err != nil && !normalPeerServeTermination(err) {
				o.emit(Event{Kind: "serve_failed", PeerID: peer.EndpointID, Err: err})
			}
		}()
	}
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
