package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// coordinatorFailure retains original independent causes without exposing peer
// names, addresses, transport text, or private payloads in presentation.
type coordinatorError struct {
	message string
	causes  []error
}

func (e coordinatorError) Error() string { return e.message }

func (e coordinatorError) Unwrap() []error { return e.causes }

func coordinatorFailure(message string, causes ...error) error {
	retained := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			retained = append(retained, cause)
		}
	}
	if len(retained) == 0 {
		return nil
	}
	return coordinatorError{message: message, causes: retained}
}

// Operation provenance survives joined cleanup failures without exposing the
// guard's response text, peer names, addresses, or certificate material.
type coordinatorOperationError struct {
	operation string
	cause     error
}

func (e coordinatorOperationError) Error() string { return "local machine access operation failed" }

func (e coordinatorOperationError) Unwrap() error { return e.cause }

func (e coordinatorOperationError) DiagnosticStage() string {
	switch e.operation {
	case "native_listener", "browser_http_listener", "browser_https_listener":
		return "listener_bind"
	case "browser_certificate":
		return "grant_issue"
	default:
		return "reconciliation"
	}
}

func (e coordinatorOperationError) DiagnosticCode() string {
	return "local_access_" + e.operation + "_failed"
}

func coordinatorOperationFailure(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return coordinatorOperationError{operation: operation, cause: cause}
}

type guardedNameClient interface {
	Acquire(context.Context, string, netip.Addr, int) (net.Listener, error)
	ReplaceNames(context.Context, map[string]netip.Addr) error
	ReplaceAliases(context.Context, map[string]string) error
	Status(context.Context) (machineguard.RuntimeStatus, error)
	Close() error
}

type CoordinatorConfig struct {
	LocalAccess          config.LocalAccessConfig
	SocketAddress        string
	NameClient           guardedNameClient
	AccountID            string // the signed-in account this daemon serves; CLI requests must carry it
	AccountLabel         string
	ConnectNameClient    func(context.Context) (guardedNameClient, error)
	DialMachine          func(context.Context, string, int) (net.Conn, error)
	ApprovePeer          func(context.Context, string, bool) error
	ReconcileTimeout     time.Duration
	ReportReconcileError func(error)
	IssueCertificate     func(context.Context, string, string, int) (tls.Certificate, error)
}

type guardedRoute struct {
	listener  net.Listener
	machineID string
	port      int
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	closing   bool
}

func (r *guardedRoute) close() error {
	r.closeOnce.Do(func() {
		r.cancelNow()
		r.closeErr = lifecycleCleanup(r.listener.Close())
		r.wg.Wait()
	})
	return r.closeErr
}

func (r *guardedRoute) cancelNow() {
	r.mu.Lock()
	if !r.closing {
		r.closing = true
		r.cancel()
	}
	r.mu.Unlock()
}

func (r *guardedRoute) startForward() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.wg.Add(1)
	return true
}

type guardedWebRoute struct {
	proxy         *splitdns.Proxy
	base          string
	machineID     string
	browserRoutes map[string]splitdns.BrowserRoute
	state         *browserGatewayState
	ports         map[int]bool // machine ports with a per-port listener
}

// browserGatewayState is the running browser gateway's current routes, read by its certificate,
// revocation and dial callbacks; a route change swaps it without restarting the gateway.
type browserGatewayState struct {
	mu        sync.RWMutex
	routes    map[string]splitdns.BrowserRoute
	byAddress map[string]string
	denied    map[string]bool
}

func (s *browserGatewayState) set(routes map[string]splitdns.BrowserRoute, byAddress map[string]string, denied map[string]bool) {
	s.mu.Lock()
	s.routes, s.byAddress, s.denied = routes, byAddress, denied
	s.mu.Unlock()
}

func (s *browserGatewayState) get() (map[string]splitdns.BrowserRoute, map[string]string, map[string]bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.routes, s.byAddress, s.denied
}

type limitedListener struct {
	net.Listener
	permits chan struct{}
	ctx     context.Context
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.permits <- struct{}{}:
			return &limitedConn{Conn: conn, release: func() { <-l.permits }}, nil
		default:
			if err := lifecycleCleanup(conn.Close()); err != nil {
				errorreport.Current().ObserveFailure(l.ctx, "paperboat-daemon", "local_gateway", "delivery", "local_access_failed", err)
			}
		}
	}
}

type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

var errCoordinatorCleanupRunning = errors.New("previous local access cleanup is still running; wait for cleanup, then retry")

type generationCleanup struct {
	done chan struct{}
	err  error
}
