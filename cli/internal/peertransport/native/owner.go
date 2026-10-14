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
	"io"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=native-regional-carrier
	"tailscale.com/tailcfg"
)

var ErrInvalid = errors.New("invalid native session")

const PrivateHTTP3ALPN = peerquic.PrivateHTTP3ALPN

type Event struct {
	Kind   string
	PeerID string
	Path   string
	Err    error
}

type peerOperationFailure struct {
	stage     string
	code      string
	err       error
	retryable bool
}

func (e *peerOperationFailure) Error() string {
	if e == nil {
		return "native peer operation failed"
	}
	switch e.stage {
	case "peer_authority":
		return "native peer authority check failed"
	case "peer_connect":
		return "native private peer connection failed"
	case "listener_accept":
		return "native peer listener could not accept a connection"
	default:
		return "native peer operation failed"
	}
}
func (e *peerOperationFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// NativeTransportRetryable distinguishes an operation-owned transport stop
// from cancellation of its caller. Only operational leaves can receive it.
func (e *peerOperationFailure) NativeTransportRetryable() bool { return e != nil && e.retryable }

func (e *peerOperationFailure) DiagnosticStage() string {
	if e == nil {
		return ""
	}
	return e.stage
}
func (e *peerOperationFailure) DiagnosticCode() string {
	if e == nil {
		return ""
	}
	return e.code
}

func classifyPeerOperationFailure(ctx context.Context, stage, code string, err error) error {
	if err == nil {
		return nil
	}
	retryable := ctx != nil && stage == "peer_connect" && peerFailureLeavesMatch(err, true)
	if ctx != nil && ctx.Err() != nil {
		cause := context.Cause(ctx)
		deadlineCause := false
		if cause != nil {
			causeType := reflect.TypeOf(cause)
			deadlineCause = causeType.Comparable() && cause == context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) && !deadlineCause && peerFailureLeavesMatch(err, false) {
			if cause != nil {
				causeType := reflect.TypeOf(cause)
				if causeType.Comparable() && cause == ctx.Err() {
					return cause
				}
			}
			return errors.Join(ctx.Err(), cause)
		}
		// Keep the known phase on deadline failures while preserving the
		// caller's deadline cause alongside the transport error.
		err = errors.Join(err, ctx.Err(), cause)
	}
	return &peerOperationFailure{stage: stage, code: code, err: err, retryable: retryable}
}

func normalPeerServeTermination(err error) bool { return peerFailureLeavesMatch(err, false) }

func peerFailureLeavesMatch(err error, deadline bool) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	if err == nil {
		return true
	}
	const maximumErrorNodes = 16
	queue := []error{err}
	visited := 0
	for len(queue) != 0 {
		if visited >= maximumErrorNodes {
			return false
		}
		current := queue[0]
		queue = queue[1:]
		visited++
		if current == nil {
			return false
		}
		typeOf := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			causes := wrapped.Unwrap()
			if len(causes) == 0 || len(causes) > maximumErrorNodes-visited-len(queue) {
				return false
			}
			added := false
			for _, cause := range causes {
				if cause != nil {
					queue = append(queue, cause)
					added = true
				}
			}
			if !added {
				return false
			}
		case interface{ Unwrap() error }:
			if cause := wrapped.Unwrap(); cause != nil {
				if visited+len(queue)+1 > maximumErrorNodes {
					return false
				}
				queue = append(queue, cause)
			} else if !normalPeerServeLeaf(current, typeOf, deadline) {
				return false
			}
		default:
			if !normalPeerServeLeaf(current, typeOf, deadline) {
				return false
			}
		}
	}
	return true
}

func normalPeerServeLeaf(err error, typeOf reflect.Type, deadline bool) bool {
	if !typeOf.Comparable() {
		return false
	}
	return err == io.EOF || err == net.ErrClosed || err == context.Canceled || deadline && err == context.DeadlineExceeded
}

type Config struct {
	Authority        *tailnet.Authority
	TLS              *tls.Config
	Observe          func(Event)
	RefreshAuthority func(context.Context) error
	MeterIncoming    func(net.Conn, MeterBinding) net.Conn
}

type Owner struct {
	authority        *tailnet.Authority
	tls              *tls.Config
	observe          func(Event)
	refreshAuthority func(context.Context) error
	meterIncoming    func(net.Conn, MeterBinding) net.Conn
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	closed           bool
	sessions         map[*Session]struct{}
	workers          sync.WaitGroup
}

func NewOwner(config Config) (*Owner, error) {
	if config.Authority == nil || config.TLS == nil || len(config.TLS.Certificates) == 0 {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Owner{authority: config.Authority, tls: config.TLS.Clone(), observe: config.Observe, refreshAuthority: config.RefreshAuthority, meterIncoming: config.MeterIncoming, ctx: ctx, cancel: cancel, sessions: make(map[*Session]struct{})}, nil
}

func (o *Owner) emit(event Event) {
	if o.observe != nil {
		o.observe(event)
	}
}

func (o *Owner) dialFailure(ctx context.Context, peerID, stage, code string, err error) error {
	failure := classifyPeerOperationFailure(ctx, stage, code, err)
	if ctx == nil || ctx.Err() == nil || !peerFailureLeavesMatch(failure, false) {
		o.emit(Event{Kind: "dial_failed", PeerID: peerID, Err: failure})
	}
	return failure
}

func (o *Owner) Dial(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	peer, err := o.authority.Peer(peerID)
	if err != nil {
		return nil, o.dialFailure(ctx, peerID, "peer_authority", "peer_authority_failed", err)
	}
	client, err := o.authority.Client(descriptor, peerID)
	if err != nil {
		return nil, o.dialFailure(ctx, peerID, "peer_authority", "peer_authority_failed", err)
	}
	if err := o.authority.PrepareRegional(ctx, peerID); err != nil {
		return nil, o.dialFailure(ctx, peerID, "peer_connect", "native_private_failed", err)
	}
	socket, err := client.Dial(ctx)
	if err != nil {
		if ctx.Err() == nil {
			o.authority.InvalidateClient(client)
		}
		return nil, o.dialFailure(ctx, peerID, "peer_connect", "native_private_failed", err)
	}
	tlsConfig := boundTLS(o.tls, peer, false)
	if class == peerquic.ClassPreview {
		tlsConfig.NextProtos = []string{PrivateHTTP3ALPN}
	}
	quicSession, err := peerquic.DialPacket(ctx, socket, tlsConfig, peerquic.DevelopmentSessionConfig(class))
	if err != nil {
		// DialPacket owns this socket, including failed TLS setup. A failed
		// handshake does not invalidate other sessions on the shared client.
		return nil, o.dialFailure(ctx, peerID, "peer_connect", "native_private_failed", err)
	}
	if current, currentErr := o.authority.Peer(peerID); currentErr != nil || current != peer {
		_ = quicSession.Close()
		if currentErr != nil {
			return nil, o.dialFailure(ctx, peerID, "peer_authority", "peer_authority_failed", errors.Join(tailnet.ErrAdmission, currentErr))
		}
		return nil, o.dialFailure(ctx, peerID, "peer_authority", "peer_authority_failed", tailnet.ErrAdmission)
	}
	session := newSession(o, peerID, peer, quicSession)
	if err := o.add(session); err != nil {
		_ = session.Close()
		return nil, err
	}
	path, _ := o.authority.PeerPath(peerID)
	o.emit(Event{Kind: "connected", PeerID: peerID, Path: path})
	return session, nil
}

func (o *Owner) Listen(ctx context.Context, region *tailcfg.DERPRegion, serve func(context.Context, *Session) error) error {
	if ctx == nil || serve == nil {
		return ErrInvalid
	}
	server, err := o.authority.Listen(region)
	if err != nil {
		return classifyPeerOperationFailure(ctx, "listener_bind", "native_private_failed", err)
	}
	if err := o.authority.PrepareRegional(ctx, ""); err != nil {
		return classifyPeerOperationFailure(ctx, "peer_connect", "native_private_failed", err)
	}
	for {
		socket, acceptErr := server.Accept(ctx)
		if acceptErr != nil {
			return classifyPeerOperationFailure(ctx, "listener_accept", "native_private_failed", acceptErr)
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
				failure := classifyPeerOperationFailure(ctx, "peer_connect", "native_private_failed", listenErr)
				if ctx.Err() == nil || !normalPeerServeTermination(failure) {
					o.emit(Event{Kind: "accept_failed", PeerID: peer.EndpointID, Err: failure})
				}
				return
			}
			defer listener.Close()
			handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
			quicSession, sessionErr := listener.Accept(handshake)
			if sessionErr != nil {
				failure := classifyPeerOperationFailure(handshake, "peer_connect", "native_private_failed", sessionErr)
				if ctx.Err() == nil || !normalPeerServeTermination(failure) {
					o.emit(Event{Kind: "accept_failed", PeerID: peer.EndpointID, Err: failure})
				}
				cancel()
				return
			}
			cancel()
			if current, currentErr := o.authority.Peer(peer.EndpointID); currentErr != nil || current != peer {
				_ = quicSession.Close()
				if currentErr != nil {
					failure := classifyPeerOperationFailure(ctx, "peer_authority", "peer_authority_failed", currentErr)
					if ctx.Err() == nil || !normalPeerServeTermination(failure) {
						o.emit(Event{Kind: "accept_failed", PeerID: peer.EndpointID, Err: failure})
					}
				}
				return
			}
			session := newSession(o, peer.EndpointID, peer, quicSession)
			if o.add(session) != nil {
				_ = session.Close()
				return
			}
			defer session.Close()
			o.emit(Event{Kind: "accepted", PeerID: peer.EndpointID})
			if serveErr := serve(o.ctx, session); !normalPeerServeTermination(serveErr) {
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
