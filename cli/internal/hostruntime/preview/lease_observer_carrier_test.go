package preview

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLeaseObserverCarrierRetriesTransportFailure(t *testing.T) {
	err := classifyLeaseObserverError(&net.DNSError{Err: "temporary lookup failure", Name: "api.example.test", IsTemporary: true})
	var retryable *RetryableCarrierError
	if !errors.As(err, &retryable) {
		t.Fatalf("network error = %v, want retryable carrier error", err)
	}
}

func TestLeaseObserverCarrierWaitsForStableHostReadiness(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	base := Lease{
		Schema: PreviewTunnelSchemaV1, Kind: PreviewLeaseKind, ID: "preview_observer_01", AccountID: "account_01", ActorID: "actor_01",
		OwnerMachineID: "host_01", OwnerSessionID: "owner_session_01", OwnerSessionKind: "foreground", Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"}, AccessMode: "public",
		Endpoint: "", EndpointState: "custom_domain_not_ready", DomainCount: 1, LeaseDeadline: now.Add(time.Hour), State: "connecting", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown",
		CreatedAt: now.Add(-time.Minute), LastRenewedAt: now, CreateOperationID: "operation_observer_01", ETag: formatLeaseETag("preview_observer_01", 1), Generation: 1,
	}
	readyLease := base
	readyLease.Endpoint = "https://preview.customer.example"
	readyLease.EndpointState = "ready"
	readyLease.State, readyLease.AllocationState, readyLease.EdgeState, readyLease.OriginState = "ready", "ready", "ready", "ready"
	readyLease.ETag = formatLeaseETag(base.ID, 2)
	readyLease.Generation = 2

	var mu sync.Mutex
	reads := 0
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{PollInterval: time.Millisecond, Reader: leaseReaderFunc(func(context.Context, string) (Lease, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads == 1 {
			return base, nil
		}
		return readyLease, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan Lease, 1)
	done := make(chan error, 1)
	go func() { done <- carrier.Run(ctx, base, func(lease Lease) error { ready <- lease; return nil }) }()
	select {
	case observed := <-ready:
		if observed.ID != base.ID || observed.Generation != 2 || !isReadyLease(observed) || observed.CreateOperationID != base.CreateOperationID {
			t.Fatalf("observed lease = %+v", observed)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not report host readiness")
	}
	if detail := carrier.ReadinessDetail(); !strings.Contains(detail, "allocation ready, edge ready, origin ready") {
		t.Fatalf("readiness detail: %s", detail)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("observer run = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not stop after context cancellation")
	}
}

func TestLeaseObserverCarrierRequiresCustomerDomainWhenProjectionHasNoURL(t *testing.T) {
	now := time.Now().UTC()
	lease := Lease{
		Schema: PreviewTunnelSchemaV1, Kind: PreviewLeaseKind, ID: "preview_observer_no_domain", AccountID: "account_01", ActorID: "actor_01",
		OwnerMachineID: "host_01", OwnerSessionID: "owner_session_01", OwnerSessionKind: "foreground", Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"}, AccessMode: "public",
		EndpointState: "custom_domain_not_ready", LeaseDeadline: now.Add(time.Hour), State: "connecting", AllocationState: "pending", EdgeState: "ready", OriginState: "ready",
		CreatedAt: now.Add(-time.Minute), LastRenewedAt: now, CreateOperationID: "operation_no_domain", ETag: formatLeaseETag("preview_observer_no_domain", 1), Generation: 1,
	}
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{Reader: leaseReaderFunc(func(context.Context, string) (Lease, error) { return lease, nil })})
	if err != nil {
		t.Fatal(err)
	}
	err = carrier.Run(context.Background(), lease, func(Lease) error { t.Fatal("unexpected readiness callback"); return nil })
	if !errors.Is(err, ErrCustomDomainRequired) {
		t.Fatalf("run error = %v, want ErrCustomDomainRequired", err)
	}
}

func TestLeaseObserverCarrierRejectsChangedLease(t *testing.T) {
	now := time.Now().UTC()
	base := Lease{Schema: PreviewTunnelSchemaV1, Kind: PreviewLeaseKind, ID: "preview_observer_02", AccountID: "account_01", ActorID: "actor_01", OwnerMachineID: "host_01", OwnerSessionID: "owner_session_01", OwnerSessionKind: "foreground", Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"}, AccessMode: "public", Endpoint: "https://preview.example.test", LeaseDeadline: now.Add(time.Hour), State: "connecting", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown", CreatedAt: now.Add(-time.Minute), LastRenewedAt: now, CreateOperationID: "operation_observer_02", ETag: formatLeaseETag("preview_observer_02", 1), Generation: 1}
	changed := base
	changed.ID = "preview_other_02"
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{Reader: leaseReaderFunc(func(context.Context, string) (Lease, error) { return changed, nil })})
	if err != nil {
		t.Fatal(err)
	}
	err = carrier.Run(context.Background(), base, func(Lease) error { return nil })
	if !errors.Is(err, ErrLeaseObserverBinding) {
		t.Fatalf("changed lease error = %v", err)
	}
}

func TestLeaseObserverCarrierRequiresReaderAndClosesIdempotently(t *testing.T) {
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{})
	if err != nil {
		t.Fatal(err)
	}
	base := Lease{ID: "preview_observer_03"}
	if err := carrier.Run(context.Background(), base, func(Lease) error { return nil }); !errors.Is(err, ErrLeaseObserverReaderRequired) {
		t.Fatalf("missing reader error = %v", err)
	}
	if err := carrier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := carrier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := carrier.SetLeaseReader(leaseReaderFunc(func(context.Context, string) (Lease, error) { return base, nil })); !errors.Is(err, ErrLeaseObserverClosed) {
		t.Fatalf("reader after close error = %v", err)
	}
}

type leaseReaderFunc func(context.Context, string) (Lease, error)

func (f leaseReaderFunc) Get(ctx context.Context, id string) (Lease, error) {
	return f(ctx, id)
}

func TestLeaseObserverOriginCheckAndCancellation(t *testing.T) {
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(nil)
	target := LeaseTarget{Scheme: "http", Address: strings.TrimPrefix(server.URL, "http://")}
	if err := carrier.CheckOrigin(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := carrier.CheckOrigin(context.Background(), target); err == nil {
		t.Fatal("closed origin passed preflight")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := carrier.CheckOrigin(ctx, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe: %v", err)
	}
	tlsServer := httptest.NewTLSServer(nil)
	defer tlsServer.Close()
	if err := carrier.CheckOrigin(context.Background(), LeaseTarget{Scheme: "https", Address: strings.TrimPrefix(tlsServer.URL, "https://")}); err == nil {
		t.Fatal("untrusted TLS origin passed preflight")
	}
}

// fakeLeaseChanges is a LeaseReader that also answers owner-machine event waits like the server: at
// once when the previews generation is above the one known, otherwise held until it moves.
type fakeLeaseChanges struct {
	mu         sync.Mutex
	lease      Lease
	reads      int
	generation int64
	moved      chan struct{}
}

func (f *fakeLeaseChanges) Get(context.Context, string) (Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.lease, nil
}

func (f *fakeLeaseChanges) WaitMachineEvents(ctx context.Context, machineID string, known map[string]int64) (map[string]int64, error) {
	for {
		f.mu.Lock()
		generation, moved := f.generation, f.moved
		f.mu.Unlock()
		if generation > known["previews"] {
			return map[string]int64{"previews": generation}, nil
		}
		select {
		case <-moved:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (f *fakeLeaseChanges) change(lease Lease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lease = lease
	f.generation++
	close(f.moved)
	f.moved = make(chan struct{})
}

// setQuietly changes the lease without moving the previews generation: edge and origin readiness do not move it.
func (f *fakeLeaseChanges) setQuietly(lease Lease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lease = lease
}

func (f *fakeLeaseChanges) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// A ready preview used to re-read its lease every 250 ms for its whole life (~2 GET/s per preview on
// production). Once ready it now reads only when the owner machine's previews topic moves.
func TestLeaseObserverCarrierReadsReadyLeaseOnlyOnChange(t *testing.T) {
	now := time.Now().UTC()
	ready := Lease{
		Schema: PreviewTunnelSchemaV1, Kind: PreviewLeaseKind, ID: "preview_observer_idle", AccountID: "account_01", ActorID: "actor_01",
		OwnerMachineID: "host_01", OwnerSessionID: "owner_session_01", OwnerSessionKind: "foreground", Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"}, AccessMode: "public",
		Endpoint: "https://preview.example.test", EndpointState: "ready", LeaseDeadline: now.Add(time.Hour), State: "ready", AllocationState: "ready", EdgeState: "ready", OriginState: "ready",
		CreatedAt: now.Add(-time.Minute), LastRenewedAt: now, CreateOperationID: "operation_observer_idle", ETag: formatLeaseETag("preview_observer_idle", 1), Generation: 1,
	}
	fake := &fakeLeaseChanges{lease: ready, generation: 7, moved: make(chan struct{})}
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{PollInterval: time.Millisecond, Reader: fake})
	if err != nil {
		t.Fatal(err)
	}
	readyCalls := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- carrier.Run(context.Background(), ready, func(Lease) error {
			select {
			case readyCalls <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-readyCalls:
	case <-time.After(time.Second):
		t.Fatal("observer did not report readiness")
	}
	time.Sleep(500 * time.Millisecond)
	// One read reached readiness and one followed the first wait answer (the generation baseline).
	if reads := fake.readCount(); reads > 2 {
		t.Fatalf("ready lease read %d times in 500 ms with nothing changing, want at most 2", reads)
	}
	stopped := ready
	stopped.State, stopped.Endpoint, stopped.EndpointState = "stopped", "", "ended"
	fake.change(stopped)
	select {
	case err := <-done:
		if !errors.Is(err, ErrLeaseObserverTerminal) {
			t.Fatalf("observer run after stop = %v, want terminal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not see the stop the previews topic announced")
	}
}

func TestLeaseObserverPendingPollBacksOff(t *testing.T) {
	delay := defaultLeaseObserverPoll
	var waited time.Duration
	for i := 0; i < 20; i++ {
		waited += delay
		delay = nextLeaseObserverDelay(delay, defaultLeaseObserverPoll)
	}
	// 250 ms, 500 ms, 1 s, then every 2 s (after eight quick reads): far fewer reads than a fixed 250 ms.
	if delay != leaseObserverPendingCap || waited != 35750*time.Millisecond {
		t.Fatalf("delay=%v waited=%v", delay, waited)
	}
	if got := nextLeaseObserverDelay(5*time.Second, 5*time.Second); got != 5*time.Second {
		t.Fatalf("an interval above the cap must stay as configured, got %v", got)
	}
}

// The previews topic does not move when a pending preview becomes ready (only insert, stop, access and target
// changes move it), so a pending lease is polled: waiting for the topic left `pb preview --background` waiting 30 s
// for readiness that had landed (end-to-end core, 2026-11-13).
func TestLeaseObserverCarrierPollsPendingLeaseWithoutTopicMove(t *testing.T) {
	now := time.Now().UTC()
	pending := Lease{
		Schema: PreviewTunnelSchemaV1, Kind: PreviewLeaseKind, ID: "preview_observer_pending", AccountID: "account_01", ActorID: "actor_01",
		OwnerMachineID: "machine_01", OwnerSessionID: "session_01", OwnerSessionKind: "terminal", Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public",
		EndpointState: "pending", LeaseDeadline: now.Add(time.Hour), State: "connecting", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown",
		CreatedAt: now.Add(-time.Minute), LastRenewedAt: now, CreateOperationID: "operation_observer_pending", ETag: formatLeaseETag("preview_observer_pending", 1), Generation: 1,
	}
	fake := &fakeLeaseChanges{lease: pending, generation: 7, moved: make(chan struct{})}
	carrier, err := NewLeaseObserverCarrier(LeaseObserverCarrierConfig{PollInterval: time.Millisecond, Reader: fake})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	readyCalls := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- carrier.Run(ctx, pending, func(Lease) error { readyCalls <- struct{}{}; return nil })
	}()
	time.Sleep(100 * time.Millisecond)
	ready := pending
	ready.State, ready.AllocationState, ready.EdgeState, ready.OriginState = "ready", "ready", "ready", "ready"
	ready.Endpoint, ready.EndpointState = "https://preview.example.test", "ready"
	ready.ETag, ready.Generation = formatLeaseETag("preview_observer_pending", 2), 2
	fake.setQuietly(ready)
	select {
	case <-readyCalls:
	case err := <-done:
		t.Fatalf("observer ended before readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not report readiness that landed without a previews topic move")
	}
}
