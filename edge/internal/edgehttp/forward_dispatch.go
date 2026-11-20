package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
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

// Inject serves a visitor another node forwarded: conn carries the visitor's bytes from its ClientHello on. It applies the visitor's
// handshake rate (forward.ErrBusy over it) and dispatches the connection as forwarded, which is never forwarded again.
// It refuses (forward.ErrDraining) only once the listener has ended; a draining edge that closed its listener still serves the visitors its HTTP server accepts.
// On nil Inject owns conn.
func (l *SharedTLSListener) Inject(conn net.Conn, visitor net.Addr) error {
	if host, _, err := net.SplitHostPort(visitor.String()); err == nil {
		if _, ok := l.handshakes.Admit(host, false); !ok {
			l.observe("tls_handshake_rate", errHandshakeRate)
			return forward.ErrBusy
		}
	}
	l.mu.Lock()
	if l.ctx.Err() != nil {
		l.mu.Unlock()
		return forward.ErrDraining
	}
	l.workers.Add(1)
	l.mu.Unlock()
	l.startDispatch(conn, true, false)
	return nil
}

// forwardVisitor passes the visitor to the node that holds host. It reports whether it took ownership of the
// connection; false (no forwarding, no holder) leaves the visitor to the local paths.
func (l *SharedTLSListener) forwardVisitor(hooks *ForwardHooks, conn, replay net.Conn, host string, releaseName func()) bool {
	if hooks == nil || hooks.Forwarder == nil || len(hooks.Forwarder.Holders(host)) == 0 {
		return false
	}
	// Not a listener worker: run waits for its workers when the listener closes, and a forwarded visitor must outlive a
	// drain (the mesh ends it, not the listener).
	go func() {
		defer releaseName()
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

// hostAdmission is the listener's decision about a TLS server name: what the authority snapshot says and which path
// serves it. dispatch acts on it; Admit only asks whether it serves.
type hostAdmission struct {
	decisions           []connectorprotocol.IngressDecision
	https               bool
	infrastructureHTTPS bool
	authorizedHTTPS     bool
	selected            *connectorprotocol.IngressDecision // the TLS passthrough decision, when that is the path
}

// resolveHost decides how host is served from the current authority; false means it is denied (the connection is
// dropped). It takes no connection and waits for nothing, so dispatch and Admit share it.
func (l *SharedTLSListener) resolveHost(host string) (hostAdmission, bool) {
	ctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
	decisions, err := l.authority.SnapshotForHost(ctx, host)
	cancel()
	https := l.httpsHostname(host)
	infrastructureHTTPS := host == l.infrastructureHostname && https
	if err != nil {
		l.observe("public_tcp_reconcile", err)
		// This exact installation name is reserved for HTTPS by server authority.
		// Keep health/installation verification usable during control outages;
		// dynamic route names must never fall back when authority is unavailable.
		if !infrastructureHTTPS {
			return hostAdmission{}, false
		}
		decisions = nil
	}
	// Customer aliases need ingress authority even when the route matcher names
	// a different host. Configuration membership cannot grant TLS ownership;
	// only live server-admitted runtime/private carriers supply that exception.
	authorizedHTTPS := l.admittedHTTPSHostname != nil && l.admittedHTTPSHostname(host)
	for _, decision := range decisions {
		if decision.Binding.Protocol == "http" && decision.Binding.Hostname == host {
			if decision.Validate(time.Now().UTC()) != nil {
				return hostAdmission{}, false
			}
			authorizedHTTPS = true
		}
	}
	// A route an operator took down still gets TLS: the HTTP layer answers it with the takedown page
	// (451 or 403) rather than a dead connection.
	if !authorizedHTTPS && https {
		if taken, ok := l.authority.(interface{ Withheld(string) bool }); ok && taken.Withheld(host) {
			authorizedHTTPS = true
		}
	}
	// A preview whose owner machine is asleep gets TLS for its "machine offline" page. Its carrier is gone, so the
	// route matcher no longer names it (https is false): the server's offline list is the authority for the name.
	if !authorizedHTTPS {
		if offline, ok := l.authority.(interface{ OwnerOffline(string) bool }); ok && offline.OwnerOffline(host) {
			authorizedHTTPS = true
		}
	}
	// The locally admitted public preview may become ready after the cached
	// complete snapshot was fetched. Refresh its exact authority before any TLS
	// writes, then inspect every binding for this hostname for protocol conflicts.
	if !authorizedHTTPS && !infrastructureHTTPS && https && l.previewHTTPSAuthority != nil {
		refresh, stop := context.WithTimeout(l.ctx, 2*time.Second)
		defer stop()
		if err := l.previewHTTPSAuthority(refresh, host); err == nil {
			decisions, err = l.authority.SnapshotForHost(refresh, host)
			if err != nil {
				return hostAdmission{}, false
			}
			for _, decision := range decisions {
				if decision.Binding.Protocol == "http" && decision.Binding.Hostname == host {
					if decision.Validate(time.Now().UTC()) != nil {
						return hostAdmission{}, false
					}
					authorizedHTTPS = true
				}
			}
		}
	}
	var selected *connectorprotocol.IngressDecision
	for _, d := range decisions {
		if d.Binding.Protocol != "tls" || d.Binding.Hostname != host {
			continue
		}
		// Invalid/expired TLS authority is a denial, never an HTTPS fallback.
		if https || authorizedHTTPS || selected != nil || d.Binding.PublicPort != 443 || d.Binding.ListenerID != "listener_tls_443" || d.Validate(time.Now().UTC()) != nil {
			return hostAdmission{}, false
		}
		current := d
		selected = &current
	}
	return hostAdmission{decisions: decisions, https: https, infrastructureHTTPS: infrastructureHTTPS, authorizedHTTPS: authorizedHTTPS, selected: selected}, true
}

// ServerName is the SNI of a visitor's ClientHello, by the listener's own inspection. It reads memory only, so it
// does not depend on the listener's context: a draining listener must still be able to tell draining from not-held.
func (l *SharedTLSListener) ServerName(hello []byte) (string, error) {
	_, info, err := inspectClientHello(context.Background(), &helloBytesConn{r: bytes.NewReader(hello)}, l.infrastructureHostname, false)
	return info.host, err
}

// Admit reports whether this listener serves host now, by dispatch's own authority decision, before any visitor is
// acknowledged (a hold for a route that is not yet attached is not waited for: the forwarder tries another node).
func (l *SharedTLSListener) Admit(host string) error {
	if l.ctx.Err() != nil {
		return forward.ErrDraining // as Inject: an ended listener is draining, not a holder that lost the name
	}
	adm, ok := l.resolveHost(host)
	if !ok || !(adm.selected != nil || adm.authorizedHTTPS || adm.infrastructureHTTPS) {
		return errNotServed
	}
	return nil
}

var errNotServed = errors.New("this node does not serve the name")

// helloBytesConn feeds a captured ClientHello to inspectClientHello; it never writes or blocks.
type helloBytesConn struct{ r *bytes.Reader }

func (c *helloBytesConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (*helloBytesConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (*helloBytesConn) Close() error                     { return nil }
func (*helloBytesConn) CloseWrite() error                { return nil }
func (*helloBytesConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*helloBytesConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*helloBytesConn) SetDeadline(time.Time) error      { return nil }
func (*helloBytesConn) SetReadDeadline(time.Time) error  { return nil }
func (*helloBytesConn) SetWriteDeadline(time.Time) error { return nil }

// ServerName and Admit on PublicTCPListeners serve the shared TLS listener; without one the node serves nothing.
func (p *PublicTCPListeners) ServerName(hello []byte) (string, error) {
	if shared := p.shared(); shared != nil {
		return shared.ServerName(hello)
	}
	return "", errNotServed
}

func (p *PublicTCPListeners) Admit(host string) error {
	if shared := p.shared(); shared != nil {
		return shared.Admit(host)
	}
	return errNotServed
}

func (p *PublicTCPListeners) shared() *SharedTLSListener {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sharedTLS
}
