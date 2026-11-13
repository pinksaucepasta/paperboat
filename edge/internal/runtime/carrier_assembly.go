package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

var (
	ErrCarrierAssemblyInvalid = errors.New("invalid data carrier assembly")
	ErrCarrierAssemblyClosed  = errors.New("data carrier assembly is closed")
)

const (
	defaultCarrierHandlers = 64
	maxCarrierHandlers     = 1024
)

// CarrierService is the lifecycle boundary implemented by
// datacarrier.Service. Keeping the narrow interface here lets deterministic
// runtime tests exercise accept and shutdown without constructing certificates
// or binding a real listener.
type CarrierService interface {
	Accept(context.Context) (*datacarrier.Server, error)
	Close() error
}

// CarrierHandler receives a carrier only after datacarrier.Service has
// completed mutual TLS/QUIC authentication and peer identity binding. The
// handler owns control/routing admission; this assembly owns the peer
// lifetime and never invents an identity or route policy.
type CarrierHandler func(context.Context, *datacarrier.Server) error

type CarrierAssemblyConfig struct {
	Reporter        *reporting.Reporter
	Service         CarrierService
	Handle          CarrierHandler
	MaximumHandlers int
	ObserveError    func(error)
	Telemetry       *datacarrier.CarrierTelemetry
	// Goaway, when set, tells one carrier's host that this edge is going away until deadline
	// (Drain sends it to every carrier).
	Goaway func(ctx context.Context, server *datacarrier.Server, deadline time.Time) error
}

// CarrierAssembly owns the accepted datacarrier.Service peers and provides a
// bounded handler boundary for edge control and route ownership. The service
// is already listening when it is passed to this constructor, as required by
// datacarrier.NewService; Start only begins consumption of authenticated
// peers.
type CarrierAssembly struct {
	reporter        *reporting.Reporter
	service         CarrierService
	handle          CarrierHandler
	observeError    func(error)
	telemetry       *datacarrier.CarrierTelemetry
	goaway          func(context.Context, *datacarrier.Server, time.Time) error
	maximumHandlers int
	// live is every carrier a handler currently serves.
	live map[*datacarrier.Server]struct{}

	mu         sync.Mutex
	started    bool
	closed     bool
	cancel     context.CancelFunc
	acceptDone chan struct{}
	handlers   sync.WaitGroup
	done       chan error
	once       sync.Once
}

func NewCarrierAssembly(config CarrierAssemblyConfig) (*CarrierAssembly, error) {
	if config.Service == nil || config.Handle == nil {
		return nil, ErrCarrierAssemblyInvalid
	}
	if config.MaximumHandlers == 0 {
		config.MaximumHandlers = defaultCarrierHandlers
	}
	if config.MaximumHandlers < 1 || config.MaximumHandlers > maxCarrierHandlers {
		return nil, ErrCarrierAssemblyInvalid
	}
	return &CarrierAssembly{
		reporter: config.Reporter, service: config.Service, handle: config.Handle, observeError: config.ObserveError,
		telemetry: config.Telemetry, goaway: config.Goaway, maximumHandlers: config.MaximumHandlers, done: make(chan error, 1),
		live: map[*datacarrier.Server]struct{}{},
	}, nil
}

// NewDataCarrierAssembly makes the production ownership explicit while
// retaining NewCarrierAssembly's narrow interface for tests and adapters.
func NewDataCarrierAssembly(service *datacarrier.Service, handle CarrierHandler, maximumHandlers int, observeError func(error)) (*CarrierAssembly, error) {
	return NewDataCarrierAssemblyWithTelemetry(service, handle, maximumHandlers, observeError, nil)
}

// NewDataCarrierAssemblyWithTelemetry keeps the authenticated accept boundary
// in the runtime composition while leaving datacarrier's transport semantics
// unchanged. The legacy constructor delegates here with telemetry disabled.
func NewDataCarrierAssemblyWithTelemetry(service *datacarrier.Service, handle CarrierHandler, maximumHandlers int, observeError func(error), telemetry *datacarrier.CarrierTelemetry) (*CarrierAssembly, error) {
	return NewCarrierAssembly(CarrierAssemblyConfig{Service: service, Handle: handle, MaximumHandlers: maximumHandlers, ObserveError: observeError, Telemetry: telemetry})
}

func (a *CarrierAssembly) Start(ctx context.Context) error {
	if a == nil || ctx == nil {
		return ErrCarrierAssemblyInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.service == nil || a.handle == nil || a.closed {
		return ErrCarrierAssemblyClosed
	}
	if a.started {
		return ErrCarrierAssemblyInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.acceptDone = make(chan struct{})
	a.started = true
	go a.acceptLoop(runCtx)
	return nil
}

func (a *CarrierAssembly) acceptLoop(ctx context.Context) {
	defer close(a.acceptDone)
	limits := make(chan struct{}, a.maximumHandlers)
	for {
		server, err := a.service.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, datacarrier.ErrCarrierClosed) {
				return
			}
			a.report(ctx, err, true)
			return
		}
		if server == nil {
			continue
		}
		var accepted io.ReadWriteCloser
		if a.telemetry != nil {
			accepted, err = a.telemetry.Accept(ctx, carrierPeerStreamInfo(server.Identity()), func(context.Context) (io.ReadWriteCloser, error) {
				// The authenticated server is the lifetime owner at this
				// boundary. Its data streams remain owned by the existing
				// handler, so the adapter deliberately does not read, write, or
				// close the server itself.
				return carrierPeerLifetime{}, nil
			})
			if err != nil {
				_ = server.Close()
				if ctx.Err() == nil {
					a.report(ctx, err, true)
				}
				return
			}
		}
		select {
		case limits <- struct{}{}:
		case <-ctx.Done():
			if accepted != nil {
				_ = accepted.Close()
			}
			_ = server.Close()
			return
		}
		a.handlers.Add(1)
		a.mu.Lock()
		a.live[server] = struct{}{}
		a.mu.Unlock()
		go func() {
			defer a.handlers.Done()
			defer func() {
				a.mu.Lock()
				delete(a.live, server)
				a.mu.Unlock()
			}()
			defer func() {
				if value := recover(); value != nil {
					a.reporter.CaptureFailure(ctx, "process_panic", reporting.PanicFailure{})
				}
			}()
			defer func() { <-limits }()
			if accepted != nil {
				defer accepted.Close()
			}
			defer server.Close()
			if err := a.handle(ctx, server); err != nil && ctx.Err() == nil {
				a.report(ctx, err, false)
			}
		}()
	}
}

func carrierPeerStreamInfo(identity datacarrier.Identity) datacarrier.StreamInfo {
	return datacarrier.StreamInfo{
		IDs: edgetelemetry.SafeIDs{
			AccountID:   identity.AccountID,
			HostID:      identity.HostID,
			TunnelID:    identity.TunnelID,
			ConnectorID: identity.ConnectorID,
			SessionID:   identity.SessionID,
		},
		Generations: edgetelemetry.Generations{
			Connector: identity.ProcessGeneration,
			Session:   identity.Generation,
		},
		CorrelationID:      "correlation_" + uuid.NewString(),
		Kind:               "carrier",
		ReadDirection:      "ingress",
		WriteDirection:     "egress",
		CancellationReason: "shutdown",
	}
}

// carrierPeerLifetime is a zero-copy lifetime marker for an authenticated
// carrier peer. Actual stream I/O remains in the existing handlers; keeping
// this adapter inert avoids changing their ordering or cancellation behavior.
type carrierPeerLifetime struct{}

func (carrierPeerLifetime) Read([]byte) (int, error)  { return 0, io.EOF }
func (carrierPeerLifetime) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (carrierPeerLifetime) Close() error              { return nil }

func (a *CarrierAssembly) report(ctx context.Context, err error, fatal bool) {
	if err == nil {
		return
	}
	if a.observeError != nil {
		a.observeError(err)
	}
	if !fatal {
		slog.Warn("carrier_handler_failed", "error", err.Error())
		observeWorkerFailure(ctx, a.reporter, "carrier_handler", err)
		return
	}
	a.once.Do(func() {
		select {
		case a.done <- err:
		default:
		}
	})
}

// Done reports a fatal accept error. Per-peer handler failures do
// not stop accepting later authenticated carriers.
func (a *CarrierAssembly) Done() <-chan error {
	if a == nil || a.done == nil {
		ch := make(chan error)
		return ch
	}
	return a.done
}

// drainMinimum keeps an idle draining carrier open long enough for its host's replacement
// carrier to land first.
var drainMinimum = 5 * time.Second

// Drain is the first step of stopping: every carrier's host is told the edge is going away until
// deadline (edge_draining), the carriers keep serving, and Drain returns once each has no open
// stream and drainMinimum has passed, or at deadline. Hosts dial their replacement at once.
func (a *CarrierAssembly) Drain(ctx context.Context, deadline time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	servers := make([]*datacarrier.Server, 0, len(a.live))
	for server := range a.live {
		servers = append(servers, server)
	}
	a.mu.Unlock()
	if len(servers) == 0 {
		return
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if a.goaway != nil {
		var sent sync.WaitGroup
		for _, server := range servers {
			sent.Add(1)
			go func() {
				defer sent.Done()
				if err := a.goaway(ctx, server, deadline); err != nil && ctx.Err() == nil {
					a.report(ctx, err, false)
				}
			}()
		}
		sent.Wait()
	}
	earliest := time.Now().Add(drainMinimum)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		idle := true
		for _, server := range servers {
			select {
			case <-server.Done():
				continue
			default:
			}
			if server.ActiveStreams() != 0 {
				idle = false
				break
			}
		}
		if idle && !time.Now().Before(earliest) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *CarrierAssembly) Shutdown(ctx context.Context) error {
	if a == nil || ctx == nil {
		return ErrCarrierAssemblyInvalid
	}
	a.mu.Lock()
	if a.service == nil {
		a.mu.Unlock()
		return nil
	}
	cancel := a.cancel
	acceptDone := a.acceptDone
	service := a.service
	wasStarted := a.started
	a.closed = true
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	closeErr := service.Close()
	if !wasStarted {
		return closeErr
	}
	if err := waitCarrierAssembly(ctx, acceptDone); err != nil {
		return errors.Join(closeErr, err)
	}
	handlersDone := make(chan struct{})
	go func() {
		a.handlers.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
		return closeErr
	case <-ctx.Done():
		return errors.Join(closeErr, ctx.Err())
	}
}

func waitCarrierAssembly(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ Component = (*CarrierAssembly)(nil)
