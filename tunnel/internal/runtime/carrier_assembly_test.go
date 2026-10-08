package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
)

type fakeCarrierService struct {
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
	acceptErr error
}

func newFakeCarrierService() *fakeCarrierService {
	return &fakeCarrierService{closed: make(chan struct{})}
}

func (s *fakeCarrierService) Accept(ctx context.Context) (*datacarrier.Server, error) {
	if s.acceptErr != nil {
		return nil, s.acceptErr
	}
	select {
	case <-s.closed:
		return nil, datacarrier.ErrCarrierClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *fakeCarrierService) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return s.closeErr
}

func TestCarrierAssemblyOwnsAcceptAndShutdownLifecycle(t *testing.T) {
	service := newFakeCarrierService()
	handled := 0
	assembly, err := NewCarrierAssembly(CarrierAssemblyConfig{
		Service: service,
		Handle: func(context.Context, *datacarrier.Server) error {
			handled++
			return nil
		},
		MaximumHandlers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := assembly.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if handled != 0 {
		t.Fatalf("unexpected handler calls: %d", handled)
	}
	if err := assembly.Start(context.Background()); !errors.Is(err, ErrCarrierAssemblyClosed) {
		t.Fatalf("restart error = %v", err)
	}
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
}

func TestCarrierAssemblyReportsFatalAcceptError(t *testing.T) {
	fatal := errors.New("listener failed")
	service := newFakeCarrierService()
	service.acceptErr = fatal
	observed := make(chan error, 1)
	assembly, err := NewCarrierAssembly(CarrierAssemblyConfig{Service: service, Handle: func(context.Context, *datacarrier.Server) error { return nil }, ObserveError: func(err error) { observed <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-assembly.Done():
		if !errors.Is(err, fatal) {
			t.Fatalf("fatal accept error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal accept error was not reported")
	}
	select {
	case err := <-observed:
		if !errors.Is(err, fatal) {
			t.Fatalf("observed accept error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal accept observation missing")
	}
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCarrierAssemblyTelemetryWrapsAuthenticatedPeerLifetime(t *testing.T) {
	serverSession := newTestCarrierSession()
	identity := datacarrier.Identity{
		AccountID: "account_1", HostID: "host_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_1",
		ProcessGeneration: 1, Generation: 1,
	}
	carrierConfig := datacarrier.DefaultConfig()
	carrierConfig.Identity = identity
	carrierConfig.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	server, err := datacarrier.NewServerWithSession(context.Background(), serverSession, carrierConfig)
	if err != nil {
		t.Fatal(err)
	}
	service := newTelemetryCarrierService(server)
	metrics := edgetelemetry.NewMetrics()
	events, err := edgetelemetry.NewEventLogWithQueue(32, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	telemetry, err := datacarrier.NewCarrierTelemetry(datacarrier.CarrierTelemetryConfig{Metrics: metrics, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	release := make(chan struct{})
	assembly, err := NewCarrierAssembly(CarrierAssemblyConfig{
		Service: service,
		Handle: func(ctx context.Context, _ *datacarrier.Server) error {
			close(handled)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Telemetry: telemetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("authenticated carrier was not handed to the runtime")
	}
	if got := runtimeMetricValue(metrics.Snapshot(), edgetelemetry.MetricActiveStreams); got != 1 {
		t.Fatalf("active authenticated carriers = %d", got)
	}
	close(release)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := assembly.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if got := runtimeMetricValue(metrics.Snapshot(), edgetelemetry.MetricActiveStreams); got != 0 {
		t.Fatalf("active authenticated carriers after shutdown = %d", got)
	}
	var accepted, closed int
	for _, event := range events.Snapshot() {
		switch event.Name {
		case "carrier_stream_accepted":
			accepted++
		case "carrier_stream_closed":
			closed++
		}
	}
	if accepted != 1 || closed != 1 {
		t.Fatalf("authenticated carrier telemetry accepted=%d closed=%d", accepted, closed)
	}
}

type testCarrierSession struct {
	closed chan struct{}
	once   sync.Once
}

func newTestCarrierSession() *testCarrierSession {
	return &testCarrierSession{closed: make(chan struct{})}
}

func (s *testCarrierSession) OpenStream(ctx context.Context) (datacarrier.StreamLink, error) {
	select {
	case <-s.closed:
		return nil, datacarrier.ErrCarrierClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *testCarrierSession) AcceptStream(ctx context.Context) (datacarrier.StreamLink, error) {
	return s.OpenStream(ctx)
}

func (s *testCarrierSession) Ping(ctx context.Context) error {
	select {
	case <-s.closed:
		return datacarrier.ErrCarrierClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *testCarrierSession) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *testCarrierSession) CloseChan() <-chan struct{} { return s.closed }

type telemetryCarrierService struct {
	accepted chan *datacarrier.Server
	closed   chan struct{}
	once     sync.Once
	server   *datacarrier.Server
}

func newTelemetryCarrierService(server *datacarrier.Server) *telemetryCarrierService {
	accepted := make(chan *datacarrier.Server, 1)
	accepted <- server
	return &telemetryCarrierService{accepted: accepted, closed: make(chan struct{}), server: server}
}

func (s *telemetryCarrierService) Accept(ctx context.Context) (*datacarrier.Server, error) {
	select {
	case server := <-s.accepted:
		return server, nil
	case <-s.closed:
		return nil, datacarrier.ErrCarrierClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *telemetryCarrierService) Close() error {
	s.once.Do(func() {
		close(s.closed)
		if s.server != nil {
			_ = s.server.Close()
		}
	})
	return nil
}

func runtimeMetricValue(samples []edgetelemetry.MetricSample, name string) uint64 {
	for _, sample := range samples {
		if sample.Name == name {
			return sample.Value
		}
	}
	return 0
}

func TestDataPlaneStopsCarrierBeforeRouteAndIngress(t *testing.T) {
	var mu sync.Mutex
	var events []string
	component := func(name string) Component { return orderedComponent{name: name, events: &events, mu: &mu} }
	dataPlane, err := NewDataPlane(DataPlaneSpec{
		Persistence: component("store"), Control: component("control"), Carrier: component("carrier"), Node: component("node"), Routes: component("routes"),
		Gateway: component("gateway"), HTTP3Ingress: component("http3"), PrivateIngress: component("private"), RedirectIngress: component("redirect"), Usage: component("usage"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:store", "start:gateway", "start:http3", "start:private", "start:redirect", "start:control", "start:node", "start:carrier", "start:routes", "start:usage", "stop:usage", "stop:routes", "stop:carrier", "stop:node", "stop:control", "stop:redirect", "stop:private", "stop:http3", "stop:gateway", "stop:store"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

type queuedCarrierService struct {
	*fakeCarrierService
	peers chan *datacarrier.Server
}

func (s *queuedCarrierService) Accept(ctx context.Context) (*datacarrier.Server, error) {
	select {
	case peer := <-s.peers:
		return peer, nil
	case <-s.closed:
		return nil, datacarrier.ErrCarrierClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestCarrierAssemblyPeerFailureRetainsAcceptAndFatalFailureReachesOwner(t *testing.T) {
	identity := datacarrier.Identity{AccountID: "account", HostID: "host", TunnelID: "tunnel", ConnectorID: "connector", SessionID: "session", ProcessGeneration: 1, Generation: 1}
	service := &queuedCarrierService{fakeCarrierService: newFakeCarrierService(), peers: make(chan *datacarrier.Server, 2)}
	failure := errors.New("handler failed")
	observed := make(chan error, 1)
	healthy := make(chan struct{})
	calls := 0
	carrier, err := NewCarrierAssembly(CarrierAssemblyConfig{Service: service, MaximumHandlers: 1, Handle: func(context.Context, *datacarrier.Server) error {
		calls++
		if calls == 1 {
			return failure
		}
		close(healthy)
		return nil
	}, ObserveError: func(err error) { observed <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := carrier.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		service.peers <- newPreviewCarrierTestServer(t, identity)
	}
	select {
	case got := <-observed:
		if !errors.Is(got, failure) {
			t.Fatal("handler cause lost")
		}
	case <-time.After(time.Second):
		t.Fatal("handler failure not observed")
	}
	select {
	case <-healthy:
	case <-time.After(time.Second):
		t.Fatal("peer failure stopped accepting")
	}
	select {
	case <-carrier.Done():
		t.Fatal("recoverable peer failure marked listener fatal")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := carrier.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	fatal := errors.New("accept owner failed")
	fs := newFakeCarrierService()
	fs.acceptErr = fatal
	fatalCarrier, err := NewCarrierAssembly(CarrierAssemblyConfig{Service: fs, Handle: func(context.Context, *datacarrier.Server) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	owner := &Assembly{done: make(chan error, 1), stop: make(chan struct{}), carrierDone: fatalCarrier.Done()}
	go owner.watchCarrier()
	if err := fatalCarrier.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-owner.Done():
		if !errors.Is(got, fatal) {
			t.Fatal("fatal accept cause lost at service assembly owner")
		}
	case <-time.After(time.Second):
		t.Fatal("fatal accept did not reach assembly owner")
	}
	close(owner.stop)
	if err := fatalCarrier.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
