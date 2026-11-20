package edgehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/addrlimit"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"

	"github.com/pinksaucepasta/paperboat/edge/internal/handover"
)

type PublicTCPAuthority interface {
	Snapshot(context.Context) ([]connectorprotocol.IngressDecision, error)
	SnapshotForHost(context.Context, string) ([]connectorprotocol.IngressDecision, error)
	ResolveDecision(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)
}

type PublicTCPListenerConfig struct {
	OnFailure             func(context.Context, string, error)
	PreviewHTTPSAuthority func(context.Context, string) error
	// RouteGrace is how long a TLS name that is not admitted yet waits to be (after a handover takeover); nil or 0 waits not.
	RouteGrace func() time.Duration
	// HoldableName says whether a name can be ours (under the edge's preview, tunnel or runtime domain); only such
	// names, and names the authority snapshot knows, wait for the grace. Nil holds none.
	HoldableName func(string) bool
	// UnknownNameTLS answers names with no live decision with the branded not-found page (wildcard fallback).
	UnknownNameTLS         *tls.Config
	ListenHost             string
	InfrastructureHostname string
	Authority              PublicTCPAuthority
	Routes                 *DataCarrierRouteRegistry
	Interval               time.Duration
	Listen                 func(string, string) (net.Listener, error)
}

// PublicTCPListeners owns only sockets authorized by the server's complete
// edge-process snapshot. Numeric ports are never allocated here.
type PublicTCPListeners struct {
	cfg        PublicTCPListenerConfig
	mu         sync.Mutex
	streams    sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	listeners  map[string]*publicTCPListener
	sharedTLS  *SharedTLSListener
	browser    *BrowserAccess
	forwarding *ForwardHooks
	// forward carries one accepted connection; tests replace it.
	forward func(context.Context, net.Conn, connectorprotocol.IngressDecision, func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)) error
	// sources bounds one visitor address's open connections. A publication is never capped: how many visitors a
	// user's service serves is theirs to decide; resource alerts watch node capacity.
	sources *addrlimit.Limiter
}

// SetForwarding makes the shared TLS listener forward to other nodes (and take their forwarded visitors). It may be
// called before or after WrapTLSListener.
func (p *PublicTCPListeners) SetForwarding(hooks ForwardHooks) {
	p.mu.Lock()
	p.forwarding = &hooks
	shared := p.sharedTLS
	p.mu.Unlock()
	if shared != nil {
		shared.SetForwarding(hooks)
	}
}

// Inject hands a visitor another node forwarded to the shared TLS listener (SharedTLSListener.Inject); before the
// listener exists the node cannot serve it, which a forwarder reads as draining.
func (p *PublicTCPListeners) Inject(conn net.Conn, visitor net.Addr) error {
	p.mu.Lock()
	shared := p.sharedTLS
	p.mu.Unlock()
	if shared == nil {
		return forward.ErrDraining
	}
	return shared.Inject(conn, visitor)
}

// SetBrowserAccess makes the unknown-name page of listeners wrapped afterwards the one the HTTP handler answers
// (nil keeps the plain not-found page). Call it before WrapTLSListener.
func (p *PublicTCPListeners) SetBrowserAccess(b *BrowserAccess) {
	p.mu.Lock()
	p.browser = b
	p.mu.Unlock()
}

type publicTCPListener struct {
	listener      net.Listener
	decision      connectorprotocol.IngressDecision
	cancel        context.CancelFunc
	done          chan struct{}
	preparedUntil time.Time
	// forwardPort is non-zero for a port this node listens on only to forward its visitors to the node holding the
	// tunnel (the peer directory names the port); a local decision for the port takes the socket over.
	forwardPort uint16
}

// forwardBindBatch bounds the directory ports bound in one reconcile tick: a node that learns 50,000 ports at
// once (a cold start, a seed refresh) must not stall the loop (and every other tunnel's accept) binding them all in
// one pass; the rest follow on the next ticks.
const forwardBindBatch = 2000

func NewPublicTCPListeners(cfg PublicTCPListenerConfig) (*PublicTCPListeners, error) {
	if cfg.Authority == nil || cfg.Routes == nil || net.ParseIP(cfg.ListenHost) == nil {
		return nil, errors.New("public TCP listener configuration is invalid")
	}
	if cfg.Interval == 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Interval < 100*time.Millisecond {
		return nil, errors.New("public TCP listener bounds are invalid")
	}
	if cfg.Listen == nil {
		cfg.Listen = handover.Listen
	}
	return &PublicTCPListeners{cfg: cfg, listeners: make(map[string]*publicTCPListener), forward: cfg.Routes.ForwardPublicTCP,
		sources: addrlimit.NewInFlight(addrlimit.Streams)}, nil
}

// PrepareTCPRoutes binds every reserved socket before the route worker reports
// the assignment ready. The authority snapshot subsequently supplies the
// exact forwarding decision; connections arriving before that are denied.
func (p *PublicTCPListeners) PrepareTCPRoutes(ctx context.Context, rules []route.RouteRule) error {
	if p == nil || ctx == nil {
		return errors.New("public TCP listeners are unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil || p.ctx.Err() != nil {
		return errors.New("public listeners are not running")
	}
	for _, rule := range rules {
		if rule.Kind == route.TunnelTLS {
			if rule.PublicTCPPort != 443 || rule.PublicTCPListenerID != "listener_tls_443" || p.sharedTLS == nil || p.sharedTLS.ctx.Err() != nil {
				return errors.New("TLS routing requires the ready shared HTTPS listener on TCP 443")
			}
			continue
		}
		if rule.Kind != route.TunnelTCP || rule.PublicTCPListenerID == "" || rule.PublicTCPPort == 0 {
			return errors.New("public TCP listener assignment is invalid")
		}
		if !edgeports.PublicTCP(int(rule.PublicTCPPort)) {
			return fmt.Errorf("public TCP port %d is outside the allowed range %d-%d", rule.PublicTCPPort, edgeports.PublicTCPMin, edgeports.PublicTCPMax)
		}
		if live := p.listeners[rule.PublicTCPListenerID]; live != nil {
			if live.decision.Binding.PublicPort != rule.PublicTCPPort || live.decision.Binding.Protocol != rule.Protocol && rule.Protocol != "" {
				return errors.New("public TCP listener identity changed port")
			}
			live.preparedUntil = time.Now().Add(2 * p.cfg.Interval)
			continue
		}
		listener, err := p.cfg.Listen("tcp", net.JoinHostPort(p.cfg.ListenHost, strconv.Itoa(int(rule.PublicTCPPort))))
		if err != nil {
			return fmt.Errorf("bind public TCP port %d: %w", rule.PublicTCPPort, err)
		}
		listenerCtx, cancel := context.WithCancel(p.ctx)
		protocol := "tcp"
		decision := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{ListenerID: rule.PublicTCPListenerID, PublicPort: rule.PublicTCPPort, Protocol: protocol, Audience: "public"}}
		live := &publicTCPListener{listener: listener, decision: decision, cancel: cancel, done: make(chan struct{}), preparedUntil: time.Now().Add(2 * p.cfg.Interval)}
		p.listeners[rule.PublicTCPListenerID] = live
		p.streams.Add(1)
		go p.accept(listenerCtx, live)
	}
	return nil
}

func (p *PublicTCPListeners) Start(ctx context.Context) error {
	if p == nil || ctx == nil {
		return errors.New("public TCP listeners are unavailable")
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return errors.New("public TCP listeners already started")
	}
	p.ctx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()
	p.observe(ctx, "public_tcp_reconcile", p.reconcile(ctx))
	go p.run(done)
	return nil
}

func (p *PublicTCPListeners) run(done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(p.ctx, p.cfg.Interval)
			p.observe(ctx, "public_tcp_reconcile", p.reconcile(ctx))
			cancel()
		}
	}
}

func (p *PublicTCPListeners) reconcile(ctx context.Context) error {
	decisions, err := p.cfg.Authority.Snapshot(ctx)
	if err != nil {
		p.withdrawDecisionListeners()
		return err
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].Binding.ListenerID < decisions[j].Binding.ListenerID })
	desired := make(map[string]connectorprotocol.IngressDecision)
	ports := make(map[uint16]string)
	for _, d := range decisions {
		if d.Binding.Protocol != "tcp" || d.Binding.Audience != "public" || d.Validate(time.Now().UTC()) != nil {
			continue
		}
		if !edgeports.PublicTCP(int(d.Binding.PublicPort)) {
			if p.cfg.OnFailure != nil {
				p.cfg.OnFailure(ctx, "public_tcp_port_range", fmt.Errorf("public TCP port %d is outside the allowed range", d.Binding.PublicPort))
			}
			continue
		}
		if existing, ok := ports[d.Binding.PublicPort]; ok && existing != d.Binding.ListenerID {
			p.withdrawDecisionListeners()
			return errors.New("conflicting public TCP listener authority")
		}
		ports[d.Binding.PublicPort] = d.Binding.ListenerID
		if prior, ok := desired[d.Binding.ListenerID]; ok && (prior.Binding.PublicPort != d.Binding.PublicPort || prior.Binding.Protocol != d.Binding.Protocol) {
			p.withdrawDecisionListeners()
			return errors.New("conflicting public listener authority")
		}

		if current, ok := desired[d.Binding.ListenerID]; !ok || d.AssignmentGeneration > current.AssignmentGeneration {
			desired[d.Binding.ListenerID] = d
		}
	}
	wanted := p.forwardedPorts(ports)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adoptForwardListeners(desired, ports, wanted)
	for id, live := range p.listeners {
		if live.forwardPort != 0 {
			continue
		}
		next, ok := desired[id]
		if !ok && time.Now().Before(live.preparedUntil) {
			continue
		}
		if !ok || next.Binding.PublicPort != live.decision.Binding.PublicPort || next.Binding.Protocol != live.decision.Binding.Protocol {
			live.cancel()
			_ = live.listener.Close()
			delete(p.listeners, id)
			continue
		}
		live.decision = next
		live.preparedUntil = time.Time{}
		delete(desired, id)
	}
	for id, decision := range desired {
		listener, listenErr := p.cfg.Listen("tcp", net.JoinHostPort(p.cfg.ListenHost, strconv.Itoa(int(decision.Binding.PublicPort))))
		if listenErr != nil {
			return fmt.Errorf("bind public TCP port %d: %w", decision.Binding.PublicPort, listenErr)
		}
		listenerCtx, cancel := context.WithCancel(p.ctx)
		live := &publicTCPListener{listener: listener, decision: decision, cancel: cancel, done: make(chan struct{})}
		p.listeners[id] = live
		p.streams.Add(1)
		go p.accept(listenerCtx, live)
	}
	return p.bindForwardPorts(wanted)
}

// forwardedPorts are the public ports to listen on only to forward: the ones the peer directory has a holder for and
// no local decision covers.
func (p *PublicTCPListeners) forwardedPorts(local map[uint16]string) []uint16 {
	p.mu.Lock()
	hooks := p.forwarding
	p.mu.Unlock()
	if hooks == nil {
		return nil
	}
	directory, ok := hooks.Forwarder.(interface{ TCPPorts() []uint16 })
	if !ok {
		return nil
	}
	var wanted []uint16
	for _, port := range directory.TCPPorts() {
		if _, covered := local[port]; !covered && edgeports.PublicTCP(int(port)) {
			wanted = append(wanted, port)
		}
	}
	return wanted
}

// adoptForwardListeners settles the sockets that only forward: a port a local decision now covers keeps its socket
// under the decision (a local decision wins, the accept path becomes the holder's), a port no longer wanted closes,
// and a wanted port that is already bound leaves wanted. Caller holds p.mu.
func (p *PublicTCPListeners) adoptForwardListeners(desired map[string]connectorprotocol.IngressDecision, local map[uint16]string, wanted []uint16) {
	keep := make(map[uint16]bool, len(wanted))
	for _, port := range wanted {
		keep[port] = true
	}
	for id, live := range p.listeners {
		if live.forwardPort == 0 {
			continue
		}
		if listenerID, covered := local[live.forwardPort]; covered {
			if _, bound := p.listeners[listenerID]; !bound {
				delete(p.listeners, id)
				live.forwardPort, live.decision = 0, desired[listenerID]
				p.listeners[listenerID] = live // the reconcile below adopts the decision like any bound listener's
				continue
			}
		} else if keep[live.forwardPort] {
			continue
		}
		live.cancel()
		_ = live.listener.Close()
		delete(p.listeners, id)
	}
}

// bindForwardPorts binds the wanted ports that are not bound yet, at most forwardBindBatch per call. A port another
// process holds is skipped, not fatal: the first such error is returned after the rest were tried. Caller holds p.mu.
func (p *PublicTCPListeners) bindForwardPorts(wanted []uint16) error {
	bound := make(map[uint16]bool, len(p.listeners))
	for _, live := range p.listeners {
		if live.forwardPort != 0 {
			bound[live.forwardPort] = true
		} else {
			bound[live.decision.Binding.PublicPort] = true
		}
	}
	var first error
	batch := 0
	for _, port := range wanted {
		if bound[port] {
			continue
		}
		if batch == forwardBindBatch {
			break
		}
		batch++
		listener, err := p.cfg.Listen("tcp", net.JoinHostPort(p.cfg.ListenHost, strconv.Itoa(int(port))))
		if err != nil {
			if first == nil {
				first = fmt.Errorf("bind forwarded public TCP port %d: %w", port, err)
			}
			continue
		}
		listenerCtx, cancel := context.WithCancel(p.ctx)
		live := &publicTCPListener{listener: listener, cancel: cancel, done: make(chan struct{}), forwardPort: port}
		p.listeners[nodemesh.TCPKey(port)] = live
		p.streams.Add(1)
		go p.accept(listenerCtx, live)
	}
	return first
}

func (p *PublicTCPListeners) accept(ctx context.Context, live *publicTCPListener) {
	defer p.streams.Done()
	defer close(live.done)
	for {
		connection, err := live.listener.Accept()
		if err != nil {
			p.mu.Lock()
			for id, current := range p.listeners {
				if current == live {
					delete(p.listeners, id)
					break
				}
			}
			p.mu.Unlock()
			failureCtx := ctx
			if ctx.Err() == nil {
				failureCtx = context.WithoutCancel(ctx)
			}
			live.cancel()
			_ = live.listener.Close()
			p.observe(failureCtx, "public_tcp_listener", err)
			return
		}
		p.mu.Lock()
		decision, forwardPort, hooks := live.decision, live.forwardPort, p.forwarding
		p.mu.Unlock()
		releaseSource, ok := p.admit(connection)
		if !ok {
			_ = connection.Close()
			p.observe(ctx, "public_tcp_stream", errPublicTCPOverShare)
			edgetelemetry.NoteRefusal(decision.Binding.Hostname)
			continue
		}
		p.streams.Add(1)
		go func() {
			defer p.streams.Done()
			defer releaseSource()
			if forwardPort != 0 {
				p.forwardTCP(ctx, connection, forwardPort, hooks)
				return
			}
			p.observe(ctx, "public_tcp_stream", p.forward(ctx, connection, decision, p.cfg.Authority.ResolveDecision))
		}()
	}
}

var errPublicTCPOverShare = errors.New("public TCP connection refused: its visitor address holds its full share of connections")

// admit takes one connection from the visitor address's share.
func (p *PublicTCPListeners) admit(connection net.Conn) (release func(), ok bool) {
	address, _, err := net.SplitHostPort(connection.RemoteAddr().String())
	if err != nil {
		address = connection.RemoteAddr().String()
	}
	return p.sources.Admit(address, false)
}

func (p *PublicTCPListeners) observe(ctx context.Context, phase string, err error) {
	if err == nil || p.cfg.OnFailure == nil {
		return
	}
	if requestErrorLeaves(err, func(leaf error) bool {
		return leaf == context.Canceled || leaf == io.EOF || ctxErr(ctx) == context.Canceled && (leaf == net.ErrClosed || leaf == io.ErrClosedPipe)
	}, true) {
		return
	}
	// A sole control failure was already published at the authenticated HTTP boundary.
	current := err
	for depth := 0; current != nil && depth < 8; depth++ {
		if attempt, ok := current.(*control.RequestFailure); ok && attempt != nil {
			return
		}
		wrapped, ok := current.(interface{ Unwrap() error })
		if !ok {
			break
		}
		current = wrapped.Unwrap()
	}
	p.cfg.OnFailure(ctx, phase, err)
}

// withdrawDecisionListeners closes the listeners a local decision owns. The ones that only forward stay: they depend
// on the peer directory, not on this node's authority, and a control outage must not cut their visitors.
func (p *PublicTCPListeners) withdrawDecisionListeners() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, live := range p.listeners {
		if live.forwardPort == 0 {
			live.cancel()
			_ = live.listener.Close()
			delete(p.listeners, id)
		}
	}
}

func (p *PublicTCPListeners) withdrawAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, live := range p.listeners {
		live.cancel()
		_ = live.listener.Close()
		delete(p.listeners, id)
	}
}

func (p *PublicTCPListeners) Shutdown(ctx context.Context) error {
	if p == nil || ctx == nil {
		return nil
	}
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.withdrawAll()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	stopped := make(chan struct{})
	go func() { p.streams.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// WrapTLSListener transfers dispatch ownership to the HTTPS server's listener.
// PublicTCPListeners only checks readiness; it never binds or closes TCP443.
func (p *PublicTCPListeners) WrapTLSListener(listener net.Listener, httpsHostname func(string) bool, admittedHTTPSHostname func(string) bool) (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sharedTLS != nil {
		return nil, errors.New("shared TLS listener already configured")
	}
	if listener == nil || httpsHostname == nil || p.cfg.InfrastructureHostname != "" && !validSNIHostname(p.cfg.InfrastructureHostname) && net.ParseIP(p.cfg.InfrastructureHostname) == nil {
		return nil, errors.New("shared TLS listener configuration is invalid")
	}
	shared := newSharedTLSListener(listener, p.cfg.Authority, p.cfg.Routes, httpsHostname, p.cfg.InfrastructureHostname, admittedHTTPSHostname, p.cfg.PreviewHTTPSAuthority, func(_ context.Context, phase string, cause error) {
		p.mu.Lock()
		ctx := p.ctx
		p.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		p.observe(ctx, phase, cause)
	})
	shared.ServeUnknownNamesWithPage(p.cfg.UnknownNameTLS)
	shared.browser = p.browser
	shared.routeGrace = p.cfg.RouteGrace
	shared.holdableName = p.cfg.HoldableName
	if p.forwarding != nil {
		shared.SetForwarding(*p.forwarding)
	}
	p.sharedTLS = shared
	return shared, nil
}
