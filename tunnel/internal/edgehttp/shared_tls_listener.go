package edgehttp

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
)

// SharedTLSListener dispatches the original ClientHello before any TLS writes.
// HTTPS connections are handed to net/http's existing ServeTLS implementation;
// opaque streams remain owned here through forwarding and shutdown.
type SharedTLSListener struct {
	net.Listener
	authority              PublicTCPAuthority
	routes                 *DataCarrierRouteRegistry
	httpsHostname          func(string) bool
	infrastructureHostname string
	ctx                    context.Context
	cancel                 context.CancelFunc
	ready                  chan net.Conn
	slots                  chan struct{}
	done                   chan struct{}
	workers                sync.WaitGroup
	closeOnce              sync.Once
	mu                     sync.Mutex
	acceptErr              error
	onFailure              func(context.Context, string, error)
	closeErr               error
}

func NewSharedTLSListener(listener net.Listener, authority PublicTCPAuthority, routes *DataCarrierRouteRegistry, httpsHostname func(string) bool, maximumConnections int, infrastructureHostname string) (*SharedTLSListener, error) {
	if listener == nil || authority == nil || routes == nil || httpsHostname == nil || maximumConnections < 1 || infrastructureHostname != "" && !validSNIHostname(infrastructureHostname) && net.ParseIP(infrastructureHostname) == nil {
		return nil, errors.New("shared TLS listener configuration is invalid")
	}
	return newSharedTLSListener(listener, authority, routes, httpsHostname, make(chan struct{}, maximumConnections), infrastructureHostname, nil), nil
}

func newSharedTLSListener(listener net.Listener, authority PublicTCPAuthority, routes *DataCarrierRouteRegistry, httpsHostname func(string) bool, slots chan struct{}, infrastructureHostname string, observe func(context.Context, string, error)) *SharedTLSListener {
	ctx, cancel := context.WithCancel(context.Background())
	l := &SharedTLSListener{Listener: listener, authority: authority, routes: routes, httpsHostname: httpsHostname, infrastructureHostname: infrastructureHostname, ctx: ctx, cancel: cancel, ready: make(chan net.Conn), slots: slots, done: make(chan struct{}), onFailure: observe}
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
	ctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
	decisions, err := l.authority.Snapshot(ctx)
	cancel()
	https := l.httpsHostname(host)
	if err != nil {
		l.observe("public_tcp_reconcile", err)
		// This exact installation name is reserved for HTTPS by server authority.
		// Keep health/installation verification usable during control outages;
		// dynamic route names must never fall back when authority is unavailable.
		if host != l.infrastructureHostname || !https {
			return
		}
		decisions = nil
	}
	if len(decisions) > 4096 {
		return
	}
	var selected *connectorprotocol.IngressDecision
	for _, d := range decisions {
		if d.Binding.Protocol != "tls" || d.Binding.Hostname != host {
			continue
		}
		// Invalid/expired TLS authority is a denial, never an HTTPS fallback.
		if https || selected != nil || d.Binding.PublicPort != 443 || d.Binding.ListenerID != "listener_tls_443" || d.Validate(time.Now().UTC()) != nil {
			return
		}
		current := d
		selected = &current
	}
	if selected != nil {
		replay.(*replayTLSConn).sharedListener = true
		l.observe("public_tcp_stream", l.routes.ForwardPublicTCP(l.ctx, replay, *selected, l.authority.ResolveDecision))
		return
	}
	if !https {
		return
	}
	select {
	case l.ready <- replay:
		owned = false
	case <-l.ctx.Done():
	}
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
