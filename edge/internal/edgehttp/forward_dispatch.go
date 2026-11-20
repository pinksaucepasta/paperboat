package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
)

// Forwarder passes a visitor this node cannot serve to the node that can (forward.Mesh).
type Forwarder interface {
	Holders(key string) []string
	Forward(ctx context.Context, conn net.Conn, key string) error
}

// MeshAcceptor takes an inbound node link (forward.Mesh). It owns the connection once it returns nil.
type MeshAcceptor interface{ Accept(conn net.Conn) error }

// ForwardHooks make a SharedTLSListener forward. A node that does not forward (self-hosted, forward_enabled off)
// sets none, and the listener behaves as before.
type ForwardHooks struct {
	Forwarder Forwarder
	Mesh      MeshAcceptor
}

// SetForwarding installs the forwarding hooks; it may be called while the listener serves.
func (l *SharedTLSListener) SetForwarding(hooks ForwardHooks) { l.hooks.Store(&hooks) }

// Inject serves a visitor another node forwarded: conn carries the visitor's bytes from its ClientHello on. It takes
// a dispatch slot without blocking (forward.ErrBusy when none is free), applies the visitor's handshake rate, and
// dispatches the connection as forwarded, which is never forwarded again. It refuses (forward.ErrDraining) only once
// the listener has ended; a draining edge that closed its listener still serves the visitors its HTTP server accepts.
// On nil Inject owns conn.
func (l *SharedTLSListener) Inject(conn net.Conn, visitor net.Addr) error {
	if host, _, err := net.SplitHostPort(visitor.String()); err == nil {
		if _, ok := l.handshakes.admit(host, false); !ok {
			l.observe("tls_handshake_rate", errHandshakeRate)
			return forward.ErrBusy
		}
	}
	select {
	case l.slots <- struct{}{}:
	default:
		return forward.ErrBusy
	}
	l.mu.Lock()
	if l.ctx.Err() != nil {
		l.mu.Unlock()
		<-l.slots
		return forward.ErrDraining
	}
	l.workers.Add(1)
	l.mu.Unlock()
	l.startDispatch(conn, true, false)
	return nil
}

// forwardVisitor passes the visitor to the node that holds host. It reports whether it took ownership of the
// connection; false (no forwarding, no holder, no room) leaves the visitor to the local paths.
func (l *SharedTLSListener) forwardVisitor(hooks *ForwardHooks, conn, replay net.Conn, host string, releaseName func()) bool {
	if hooks == nil || hooks.Forwarder == nil || len(hooks.Forwarder.Holders(host)) == 0 {
		return false
	}
	select {
	case l.forwarding <- struct{}{}:
	default:
		return false
	}
	// Not a listener worker: run waits for its workers when the listener closes, and a forwarded visitor must outlive a
	// drain (the mesh ends it, not the listener).
	go func() {
		defer releaseName()
		defer func() { <-l.forwarding }()
		defer conn.Close()
		if err := hooks.Forwarder.Forward(l.ctx, replay, host); err != nil {
			l.forwardFailed(replay, host, err)
		}
	}()
	return true
}

// forwardFailed reports a forward that no holder took and answers the visitor, who has been sent nothing yet.
func (l *SharedTLSListener) forwardFailed(replay net.Conn, host string, err error) {
	stage := forward.StageLookup
	var failure *forward.Failure
	if errors.As(err, &failure) {
		stage = failure.Stage
	}
	l.observe(stage, err)
	if l.unknown == nil {
		return
	}
	if errors.Is(err, forward.ErrNoHolder) {
		serveNotFoundPage(replay, l.unknown, l.browser, host)
		return
	}
	serveBrandedPage(replay, l.unknown, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		writeErrorPage(w, http.StatusServiceUnavailable, forwardUnavailableMessage)
	})
}

// serveBrandedPage answers one connection with a page over TLS, bounded to 10 s.
func serveBrandedPage(conn net.Conn, config *tls.Config, handler http.HandlerFunc) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: handler}
	server.SetKeepAlivesEnabled(false)
	_ = server.Serve(&oneConnListener{conn: tls.Server(conn, config)})
}
