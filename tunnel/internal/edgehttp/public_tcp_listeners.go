package edgehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

type PublicTCPAuthority interface {
	Snapshot(context.Context) ([]connectorprotocol.IngressDecision, error)
	ResolveDecision(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)
}

type PublicTCPListenerConfig struct {
	ListenHost             string
	InfrastructureHostname string
	Authority              PublicTCPAuthority
	Routes                 *DataCarrierRouteRegistry
	Interval               time.Duration
	MaximumListeners       int
	MaximumConnections     int
	Listen                 func(string, string) (net.Listener, error)
}

// PublicTCPListeners owns only sockets authorized by the server's complete
// edge-process snapshot. Numeric ports are never allocated here.
type PublicTCPListeners struct {
	cfg         PublicTCPListenerConfig
	mu          sync.Mutex
	streams     sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	listeners   map[string]*publicTCPListener
	connections chan struct{}
	sharedTLS   *SharedTLSListener
}

type publicTCPListener struct {
	listener      net.Listener
	decision      connectorprotocol.IngressDecision
	cancel        context.CancelFunc
	done          chan struct{}
	preparedUntil time.Time
}

func NewPublicTCPListeners(cfg PublicTCPListenerConfig) (*PublicTCPListeners, error) {
	if cfg.Authority == nil || cfg.Routes == nil || net.ParseIP(cfg.ListenHost) == nil {
		return nil, errors.New("public TCP listener configuration is invalid")
	}
	if cfg.Interval == 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.MaximumListeners == 0 {
		cfg.MaximumListeners = 4096
	}
	if cfg.MaximumConnections == 0 {
		cfg.MaximumConnections = 4096
	}
	if cfg.Interval < 100*time.Millisecond || cfg.MaximumListeners < 1 || cfg.MaximumConnections < 1 {
		return nil, errors.New("public TCP listener bounds are invalid")
	}
	if cfg.Listen == nil {
		cfg.Listen = net.Listen
	}
	return &PublicTCPListeners{cfg: cfg, listeners: make(map[string]*publicTCPListener), connections: make(chan struct{}, cfg.MaximumConnections)}, nil
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
		if live := p.listeners[rule.PublicTCPListenerID]; live != nil {
			if live.decision.Binding.PublicPort != rule.PublicTCPPort || live.decision.Binding.Protocol != rule.Protocol && rule.Protocol != "" {
				return errors.New("public TCP listener identity changed port")
			}
			live.preparedUntil = time.Now().Add(2 * p.cfg.Interval)
			continue
		}
		if len(p.listeners) >= p.cfg.MaximumListeners {
			return errors.New("public listener limit exceeded")
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
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()
	_ = p.reconcile(ctx)
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
			_ = p.reconcile(ctx)
			cancel()
		}
	}
}

func (p *PublicTCPListeners) reconcile(ctx context.Context) error {
	decisions, err := p.cfg.Authority.Snapshot(ctx)
	if err != nil {
		p.withdrawAll()
		return err
	}
	if len(decisions) > 4096 {
		p.withdrawAll()
		return errors.New("public ingress decision limit exceeded")
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].Binding.ListenerID < decisions[j].Binding.ListenerID })
	desired := make(map[string]connectorprotocol.IngressDecision)
	ports := make(map[uint16]string)
	for _, d := range decisions {
		if d.Binding.Protocol != "tcp" || d.Binding.Audience != "public" || d.Validate(time.Now().UTC()) != nil {
			continue
		}
		if existing, ok := ports[d.Binding.PublicPort]; ok && existing != d.Binding.ListenerID {
			p.withdrawAll()
			return errors.New("conflicting public TCP listener authority")
		}
		ports[d.Binding.PublicPort] = d.Binding.ListenerID
		if prior, ok := desired[d.Binding.ListenerID]; ok && (prior.Binding.PublicPort != d.Binding.PublicPort || prior.Binding.Protocol != d.Binding.Protocol) {
			p.withdrawAll()
			return errors.New("conflicting public listener authority")
		}

		if current, ok := desired[d.Binding.ListenerID]; !ok || d.AssignmentGeneration > current.AssignmentGeneration {
			desired[d.Binding.ListenerID] = d
		}
	}
	if len(desired) > p.cfg.MaximumListeners {
		p.withdrawAll()
		return errors.New("public TCP listener limit exceeded")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, live := range p.listeners {
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
	return nil
}

func (p *PublicTCPListeners) accept(ctx context.Context, live *publicTCPListener) {
	defer p.streams.Done()
	defer close(live.done)
	for {
		connection, err := live.listener.Accept()
		if err != nil {
			return
		}
		select {
		case p.connections <- struct{}{}:
			p.mu.Lock()
			decision := live.decision
			p.mu.Unlock()
			p.streams.Add(1)
			go func() {
				defer p.streams.Done()
				defer func() { <-p.connections }()

				_ = p.cfg.Routes.ForwardPublicTCP(ctx, connection, decision, p.cfg.Authority.ResolveDecision)
			}()
		default:
			_ = connection.Close()
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
func (p *PublicTCPListeners) WrapTLSListener(listener net.Listener, httpsHostname func(string) bool) (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sharedTLS != nil {
		return nil, errors.New("shared TLS listener already configured")
	}
	if listener == nil || httpsHostname == nil || p.cfg.InfrastructureHostname != "" && !validSNIHostname(p.cfg.InfrastructureHostname) && net.ParseIP(p.cfg.InfrastructureHostname) == nil {
		return nil, errors.New("shared TLS listener configuration is invalid")
	}
	// Opaque TLS streams, raw TCP streams and pending TLS inspections share
	// the existing process admission budget.
	shared := newSharedTLSListener(listener, p.cfg.Authority, p.cfg.Routes, httpsHostname, p.connections, p.cfg.InfrastructureHostname)
	p.sharedTLS = shared
	return shared, nil
}
