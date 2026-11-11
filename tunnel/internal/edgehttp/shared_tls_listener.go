package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	edgetelemetry "github.com/pinksaucepasta/paperboat/tunnel/internal/telemetry"
)

// SharedTLSListener dispatches the original ClientHello before any TLS writes.
// HTTPS connections are handed to net/http's existing ServeTLS implementation;
// opaque streams remain owned here through forwarding and shutdown.
type SharedTLSListener struct {
	net.Listener
	authority              PublicTCPAuthority
	routes                 *DataCarrierRouteRegistry
	httpsHostname          func(string) bool
	admittedHTTPSHostname  func(string) bool
	previewHTTPSAuthority  func(context.Context, string) error
	infrastructureHostname string
	ctx                    context.Context
	cancel                 context.CancelFunc
	ready                  chan net.Conn
	slots                  chan struct{}
	// passthrough bounds the TLS-passthrough streams handed off from dispatch; they are long-lived,
	// so they must not hold the dispatch slots new HTTPS connections need (audit T3).
	passthrough chan struct{}
	// https bounds the HTTPS connections handed to the HTTP server for their whole life; a dispatch
	// slot is freed at hand-off (golang.org/x/net/netutil LimitListener holds its slot until Close).
	https chan struct{}
	// forward carries one passthrough stream (the route registry's ForwardPublicTCP).
	forward   func(context.Context, net.Conn, connectorprotocol.IngressDecision, func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)) error
	done      chan struct{}
	workers   sync.WaitGroup
	closeOnce sync.Once
	mu        sync.Mutex
	acceptErr error
	onFailure func(context.Context, string, error)
	closeErr  error
	// unknown completes TLS for a name with no live decision (a stopped or unknown preview) and answers
	// only the branded "does not exist" page; it never reaches the HTTPS handler or an origin.
	unknown *tls.Config
	// routeGrace and held: right after a handover takeover a name whose host has not re-attached its carrier
	// yet is held until it is admitted or the grace ends, instead of answering the not-found page at once (anvit
	// 2026-11-09: 404s up to 6 s after every edge swap). Only a name that can be ours is held (holdableName: under
	// our preview, tunnel or runtime domain; or one the authority snapshot names), and held bounds how many.
	routeGrace   func() time.Duration
	holdableName func(string) bool
	held         *holdSet
	// handshakes is the per-address TLS handshake rate; names caps one name's share of the slots, so a
	// flood aimed at one preview cannot starve every other name (edge abuse plan, L2 and L5).
	handshakes *addressLimiter
	names      *holdSet
}

// holdSet bounds the connections held during a takeover grace: overall, per name, and with a reserve of slots only
// names the authority snapshot knows may use, so random names under our domains cannot starve a real preview.
type holdSet struct {
	mu                       sync.Mutex
	total, max, perName, rsv int
	names                    map[string]int
}

func newHoldSet(max, perName, reserve int) *holdSet {
	return &holdSet{max: max, perName: perName, rsv: reserve, names: make(map[string]int)}
}

// acquire takes one hold on name, or reports false when a bound is reached; release gives it back.
func (h *holdSet) acquire(name string, known bool) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	limit := h.max
	if !known {
		limit -= h.rsv
	}
	if h.total >= limit || h.names[name] >= h.perName {
		return nil, false
	}
	h.total++
	h.names[name]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.total--
		if h.names[name]--; h.names[name] == 0 {
			delete(h.names, name)
		}
	}, true
}

// takeoverHoldMax, takeoverHoldPerName and takeoverHoldReserve size the TLS holds: 256 at once, at most 16 per
// name, the last 64 for names the authority knows.
const (
	takeoverHoldMax     = 256
	takeoverHoldPerName = 16
	takeoverHoldReserve = 64
)

// nameShare is the most connections one TLS name may hold of the listener's slots: a quarter.
func nameShare(slots int) int { return max(1, slots/4) }

var errNameShareFull = errors.New("one name holds its full share of the edge's TLS slots")
var errHandshakeRate = errors.New("client address over its TLS handshake rate")

// ServeUnknownNamesWithPage sets the TLS config (the edge certificate selector, wildcard fallback) used to
// answer names that have no live decision with the branded not-found page instead of dropping TLS.
func (l *SharedTLSListener) ServeUnknownNamesWithPage(config *tls.Config) {
	if config != nil {
		config = config.Clone()
		config.NextProtos = []string{"http/1.1"}
	}
	l.unknown = config
}

func NewSharedTLSListener(listener net.Listener, authority PublicTCPAuthority, routes *DataCarrierRouteRegistry, httpsHostname func(string) bool, maximumConnections int, infrastructureHostname string) (*SharedTLSListener, error) {
	if listener == nil || authority == nil || routes == nil || httpsHostname == nil || maximumConnections < 1 || infrastructureHostname != "" && !validSNIHostname(infrastructureHostname) && net.ParseIP(infrastructureHostname) == nil {
		return nil, errors.New("shared TLS listener configuration is invalid")
	}
	return newSharedTLSListener(listener, authority, routes, httpsHostname, make(chan struct{}, maximumConnections), infrastructureHostname, nil, nil, nil), nil
}

func newSharedTLSListener(listener net.Listener, authority PublicTCPAuthority, routes *DataCarrierRouteRegistry, httpsHostname func(string) bool, slots chan struct{}, infrastructureHostname string, admittedHTTPSHostname func(string) bool, previewHTTPSAuthority func(context.Context, string) error, observe func(context.Context, string, error)) *SharedTLSListener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &SharedTLSListener{Listener: listener, authority: authority, routes: routes, httpsHostname: httpsHostname, admittedHTTPSHostname: admittedHTTPSHostname, previewHTTPSAuthority: previewHTTPSAuthority, infrastructureHostname: infrastructureHostname, ctx: ctx, cancel: cancel, ready: make(chan net.Conn), held: newHoldSet(takeoverHoldMax, takeoverHoldPerName, takeoverHoldReserve), slots: slots, passthrough: make(chan struct{}, cap(slots)), https: make(chan struct{}, cap(slots)), forward: routes.ForwardPublicTCP, done: make(chan struct{}), onFailure: observe,
		handshakes: newAddressLimiter(addressHandshakeRate, addressRate{}, 0), names: newHoldSet(cap(slots), nameShare(cap(slots)), 0)}
	go l.run()
	return l
}

func (l *SharedTLSListener) run() {
	defer close(l.done)
	defer l.cancel()
	defer l.workers.Wait()
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			l.mu.Lock()
			l.acceptErr = err
			l.mu.Unlock()
			l.cancel()
			return
		}
		if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			if _, ok := l.handshakes.admit(host, false); !ok {
				_ = conn.Close()
				l.observe("tls_handshake_rate", errHandshakeRate)
				continue
			}
		}
		select {
		case l.slots <- struct{}{}:
			l.workers.Add(1)
			go func() { defer l.workers.Done(); defer func() { <-l.slots }(); l.dispatch(conn) }()
		default:
			_ = conn.Close()
		}
	}
}

func (l *SharedTLSListener) dispatch(conn net.Conn) {
	owned := true
	defer func() {
		if owned {
			_ = conn.Close()
		}
	}()
	replay, host, err := inspectTLSInfrastructureClientHello(l.ctx, conn, l.infrastructureHostname)
	if err != nil {
		l.observe("tls_dispatch", err)
		return
	}
	releaseName, ok := l.names.acquire(host, true)
	if !ok {
		l.observe("tls_name_share", errNameShareFull)
		edgetelemetry.NoteRefusal(host)
		return
	}
	// The name's share is held for the connection's whole life; ownership moves with the connection.
	defer func() {
		if owned {
			releaseName()
		}
	}()
	ctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
	decisions, err := l.authority.Snapshot(ctx)
	cancel()
	https := l.httpsHostname(host)
	infrastructureHTTPS := host == l.infrastructureHostname && https
	if err != nil {
		l.observe("public_tcp_reconcile", err)
		// This exact installation name is reserved for HTTPS by server authority.
		// Keep health/installation verification usable during control outages;
		// dynamic route names must never fall back when authority is unavailable.
		if !infrastructureHTTPS {
			return
		}
		decisions = nil
	}
	if len(decisions) > 4096 {
		return
	}
	// Customer aliases need ingress authority even when the route matcher names
	// a different host. Configuration membership cannot grant TLS ownership;
	// only live server-admitted runtime/private carriers supply that exception.
	authorizedHTTPS := l.admittedHTTPSHostname != nil && l.admittedHTTPSHostname(host)
	for _, decision := range decisions {
		if decision.Binding.Protocol == "http" && decision.Binding.Hostname == host {
			if decision.Validate(time.Now().UTC()) != nil {
				return
			}
			authorizedHTTPS = true
		}
	}
	// A route that exists but is withheld only for bandwidth quota still gets TLS: the HTTP layer
	// answers it with the typed refusal (429, bandwidth_quota_exhausted) rather than a dead connection.
	if !authorizedHTTPS && https {
		if denied, ok := l.authority.(interface{ QuotaDenied(string) bool }); ok && denied.QuotaDenied(host) {
			authorizedHTTPS = true
		}
	}
	// The locally admitted public preview may become ready after the cached
	// complete snapshot was fetched. Refresh its exact authority before any TLS
	// writes, then inspect the entire current snapshot for protocol conflicts.
	if !authorizedHTTPS && !infrastructureHTTPS && https && l.previewHTTPSAuthority != nil {
		refresh, stop := context.WithTimeout(l.ctx, 2*time.Second)
		defer stop()
		if err := l.previewHTTPSAuthority(refresh, host); err == nil {
			decisions, err = l.authority.Snapshot(refresh)
			if err != nil || len(decisions) > 4096 {
				return
			}
			for _, decision := range decisions {
				if decision.Binding.Protocol == "http" && decision.Binding.Hostname == host {
					if decision.Validate(time.Now().UTC()) != nil {
						return
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
			return
		}
		current := d
		selected = &current
	}
	if selected != nil {
		// Hand the long-lived passthrough stream to its own bounded pool and free this dispatch
		// slot; a full passthrough pool refuses only this connection.
		select {
		case l.passthrough <- struct{}{}:
		default:
			return
		}
		owned = false
		decision := *selected
		replay.(*replayTLSConn).sharedListener = true
		l.workers.Add(1)
		go func() {
			defer l.workers.Done()
			defer releaseName()
			defer func() { <-l.passthrough }()
			defer conn.Close()
			l.observe("public_tcp_stream", l.forward(l.ctx, replay, decision, l.authority.ResolveDecision))
		}()
		return
	}
	if !authorizedHTTPS && !infrastructureHTTPS {
		if l.awaitAdmission(conn, host, hostInDecisions(decisions, host)) {
			authorizedHTTPS = true
		} else {
			if l.unknown != nil {
				serveNotFoundPage(replay, l.unknown)
			}
			return
		}
	}
	select {
	case l.https <- struct{}{}:
	default:
		return
	}
	owned = false
	replay.(*replayTLSConn).release = func() { <-l.https; releaseName() }
	select {
	case l.ready <- replay:
	case <-l.ctx.Done():
		_ = replay.Close()
	}
}

// awaitAdmission waits, within the takeover grace, for host to be authorized for HTTPS: admitted by its attached
// carrier, or a public preview whose attachment the server confirms with a current ingress decision (the same two
// ways dispatch authorizes a name; the first only covers restricted routes).
func (l *SharedTLSListener) awaitAdmission(conn net.Conn, host string, known bool) bool {
	if l.routeGrace == nil || l.routeGrace() <= 0 || !known && (l.holdableName == nil || !l.holdableName(host)) {
		return false
	}
	release, ok := l.held.acquire(host, known)
	if !ok {
		return false
	}
	defer release()
	ctx, cancel := context.WithTimeout(l.ctx, l.routeGrace())
	defer cancel()
	gone, stopWatching := watchClient(conn)
	defer stopWatching()
	for !(l.admittedHTTPSHostname != nil && l.admittedHTTPSHostname(host)) && !l.previewDecisionExists(ctx, host) {
		select {
		case <-ctx.Done():
			return false
		case <-gone:
			return false
		case <-time.After(routeHoldPoll):
		}
	}
	return true
}

// watchClient reports on gone when the client closes the connection or sends anything while the edge holds it:
// a client waits silently for the server hello, so a read that returns is a client that left (or broke the
// protocol). stop ends the watch and restores the connection for the handshake.
func watchClient(conn net.Conn) (gone <-chan struct{}, stop func()) {
	left, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		var b [1]byte
		if _, err := conn.Read(b[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
			close(left)
		}
	}()
	return left, func() {
		_ = conn.SetReadDeadline(time.Unix(1, 0))
		<-finished
		_ = conn.SetReadDeadline(time.Time{})
	}
}

// hostInDecisions reports whether the authority snapshot names host in any binding.
func hostInDecisions(decisions []connectorprotocol.IngressDecision, host string) bool {
	for _, d := range decisions {
		if d.Binding.Hostname == host {
			return true
		}
	}
	return false
}

// previewDecisionExists: the public preview's exact authority refreshes and the current snapshot holds its valid
// http decision.
func (l *SharedTLSListener) previewDecisionExists(ctx context.Context, host string) bool {
	if l.previewHTTPSAuthority == nil || l.previewHTTPSAuthority(ctx, host) != nil {
		return false
	}
	decisions, err := l.authority.Snapshot(ctx)
	if err != nil || len(decisions) > 4096 {
		return false
	}
	for _, decision := range decisions {
		if decision.Binding.Protocol == "http" && decision.Binding.Hostname == host {
			return decision.Validate(time.Now().UTC()) == nil
		}
	}
	return false
}

func (l *SharedTLSListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.ready:
		return conn, nil
	case <-l.ctx.Done():
		l.mu.Lock()
		err := l.acceptErr
		l.mu.Unlock()
		if err == nil {
			err = net.ErrClosed
		}
		return nil, err
	}
}

func (l *SharedTLSListener) Close() error {
	l.closeOnce.Do(func() { l.cancel(); l.closeErr = l.Listener.Close() })
	<-l.done
	return l.closeErr
}

func (l *SharedTLSListener) observe(phase string, err error) {
	if err != nil && l.onFailure != nil {
		l.onFailure(l.ctx, phase, err)
	}
}

// serveNotFoundPage answers one connection with the branded not-found page over TLS, bounded to 10 s.
// The handshake fails for a name the fallback certificate does not cover.
func serveNotFoundPage(conn net.Conn, config *tls.Config) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		writeErrorPage(w, http.StatusNotFound, notFoundMessage)
	})}
	server.SetKeepAlivesEnabled(false)
	_ = server.Serve(&oneConnListener{conn: tls.Server(conn, config)})
}

// oneConnListener hands out a single connection, then reports closed once it ends.
type oneConnListener struct {
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.done == nil {
		l.done = make(chan struct{})
		return &closeNotifyConn{Conn: l.conn, closed: func() { l.once.Do(func() { close(l.done) }) }}, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type closeNotifyConn struct {
	net.Conn
	closed func()
}

func (c *closeNotifyConn) Close() error { err := c.Conn.Close(); c.closed(); return err }
