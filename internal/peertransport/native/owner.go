// Package native owns Paperboat QUIC sessions over signed Tailcat authority.
// Application session ownership stays above the authorized carrier selection.
package native

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/tailscale/tailcat"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=native-regional-carrier
	"tailscale.com/tailcfg"
)

var ErrInvalid = errors.New("invalid native session")

const PrivateHTTP3ALPN = peerquic.PrivateHTTP3ALPN

type Event struct {
	Kind   string
	PeerID string
	Err    error
}

type Config struct {
	Authority *tailnet.Authority
	TLS       *tls.Config
	Observe   func(Event)
}

type Owner struct {
	authority *tailnet.Authority
	tls       *tls.Config
	observe   func(Event)
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	sessions  map[*Session]struct{}
	workers   sync.WaitGroup
}

func NewOwner(config Config) (*Owner, error) {
	if config.Authority == nil || config.TLS == nil || len(config.TLS.Certificates) == 0 {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Owner{authority: config.Authority, tls: config.TLS.Clone(), observe: config.Observe, ctx: ctx, cancel: cancel, sessions: make(map[*Session]struct{})}, nil
}

func (o *Owner) emit(event Event) {
	if o.observe != nil {
		o.observe(event)
	}
}

func (o *Owner) Dial(ctx context.Context, descriptor tailcat.Addr, peerID string, class peerquic.Class) (*Session, error) {
	peer, err := o.authority.Peer(peerID)
	if err != nil {
		return nil, err
	}
	client, err := o.authority.Client(descriptor, peerID)
	if err != nil {
		return nil, err
	}
	if err := o.authority.PrepareRegional(ctx, peerID); err != nil {
		return nil, err
	}
	socket, err := client.Dial(ctx)
	if err != nil {
		if ctx.Err() == nil {
			o.authority.InvalidateClient(client)
		}
		return nil, err
	}
	tlsConfig := boundTLS(o.tls, peer, false)
	if class == peerquic.ClassPreview {
		tlsConfig.NextProtos = []string{PrivateHTTP3ALPN}
	}
	quicSession, err := peerquic.DialPacket(ctx, socket, tlsConfig, peerquic.DevelopmentSessionConfig(class))
	if err != nil {
		if ctx.Err() == nil {
			o.authority.InvalidateClient(client)
		}
		o.emit(Event{Kind: "dial_failed", PeerID: peerID, Err: err})
		return nil, err
	}
	if current, currentErr := o.authority.Peer(peerID); currentErr != nil || current != peer {
		_ = quicSession.Close()
		return nil, tailnet.ErrAdmission
	}
	session := newSession(o, peerID, quicSession)
	if err := o.add(session); err != nil {
		_ = session.Close()
		return nil, err
	}
	o.emit(Event{Kind: "connected", PeerID: peerID})
	return session, nil
}

func (o *Owner) Listen(ctx context.Context, region *tailcfg.DERPRegion, serve func(context.Context, *Session) error) error {
	if ctx == nil || serve == nil {
		return ErrInvalid
	}
	server, err := o.authority.Listen(region)
	if err != nil {
		return err
	}
	if err := o.authority.PrepareRegional(ctx, ""); err != nil {
		return err
	}
	for {
		socket, acceptErr := server.Accept(ctx)
		if acceptErr != nil {
			return acceptErr
		}
		remote, parseErr := netip.ParseAddrPort(socket.RemoteAddr().String())
		peer, peerErr := o.authority.PeerAt(remote.Addr())
		if parseErr != nil || peerErr != nil {
			_ = socket.Close()
			continue
		}
		o.workers.Add(1)
		go func() {
			defer o.workers.Done()
			listenerConfig := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
			listenerConfig.AcceptsHTTP3 = true
			listener, listenErr := peerquic.ListenPacket(socket, boundTLS(o.tls, peer, true), listenerConfig)
			if listenErr != nil {
				o.emit(Event{Kind: "accept_failed", PeerID: peer.EndpointID, Err: listenErr})
				return
			}
			defer listener.Close()
			handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
			quicSession, sessionErr := listener.Accept(handshake)
			cancel()
			if sessionErr != nil {
				return
			}
			if current, currentErr := o.authority.Peer(peer.EndpointID); currentErr != nil || current != peer {
				_ = quicSession.Close()
				return
			}
			session := newSession(o, peer.EndpointID, quicSession)
			if o.add(session) != nil {
				_ = session.Close()
				return
			}
			defer session.Close()
			o.emit(Event{Kind: "accepted", PeerID: peer.EndpointID})
			if serveErr := serve(o.ctx, session); serveErr != nil && o.ctx.Err() == nil {
				o.emit(Event{Kind: "serve_failed", PeerID: peer.EndpointID, Err: serveErr})
			}
		}()
	}
}

func boundTLS(base *tls.Config, peer tailnet.NetworkBinding, server bool) *tls.Config {
	result := base.Clone()
	previous := result.VerifyConnection
	result.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		if len(state.PeerCertificates) != 1 {
			return ErrInvalid
		}
		leaf := state.PeerCertificates[0]
		now := time.Now()
		if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+time.Minute {
			return ErrInvalid
		}
		want, err := base64.RawURLEncoding.Strict().DecodeString(peer.QUICPublicKey)
		public, ok := leaf.PublicKey.(ed25519.PublicKey)
		if err != nil || len(want) != ed25519.PublicKeySize || !ok || !bytes.Equal(public, want) {
			return ErrInvalid
		}
		if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
			return ErrInvalid
		}
		return nil
	}
	if server {
		result.ClientAuth = tls.RequireAnyClientCert
		result.NextProtos = []string{peerquic.ALPN, PrivateHTTP3ALPN}
	}
	return result
}

func (o *Owner) add(session *Session) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return net.ErrClosed
	}
	o.sessions[session] = struct{}{}
	return nil
}
func (o *Owner) remove(session *Session) { o.mu.Lock(); delete(o.sessions, session); o.mu.Unlock() }

func (o *Owner) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	o.cancel()
	sessions := make([]*Session, 0, len(o.sessions))
	for session := range o.sessions {
		sessions = append(sessions, session)
	}
	o.mu.Unlock()
	var result error
	for _, session := range sessions {
		result = errors.Join(result, session.Close())
	}
	result = errors.Join(result, o.authority.Close())
	o.workers.Wait()
	return result
}
