package preview

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type resumeResolver struct {
	mu       sync.Mutex
	requests []DispatchRequest
	started  chan struct{}
}

func (r *resumeResolver) ResolvePreviewCarrier(_ context.Context, request DispatchRequest) (Carrier, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	return &sessionCarrier{closed: make(chan struct{}), run: func(ctx context.Context, _ Lease, _ func(Lease) error) error {
		select {
		case r.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}}, nil
}

// runtimeRun is one run of the host runtime: its own owner registry, lease manager and dispatcher.
type runtimeRun struct {
	registry *RuntimeOwnerSessionRegistry
	owners   *OwnerSessionLeaseManager
	manager  *DispatchManager
	leases   *sessionLeaseClient
	resolver *resumeResolver
	stop     func()
}

func startRuntimeRun(t *testing.T, store *BackgroundStore, bootID string, now time.Time, current func(context.Context, Lease) (Lease, error)) *runtimeRun {
	t.Helper()
	runtimeDone := make(chan struct{})
	registry, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{AccountID: "account_1", MachineID: "machine_1", RuntimeDone: runtimeDone})
	if err != nil {
		t.Fatal(err)
	}
	owners, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_1", ControlToken: "control_secret", Registry: registry, TTL: 10 * time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := &runtimeRun{registry: registry, owners: owners, leases: &sessionLeaseClient{}, resolver: &resumeResolver{started: make(chan struct{}, 4)}}
	run.manager, err = NewDispatchManager(DispatchManagerConfig{
		MachineID: "machine_1", Owners: registry, Leases: run.leases, Carriers: run.resolver, Readiness: &dispatchObserver{},
		Now:        func() time.Time { return now },
		Background: &BackgroundResume{Store: store, OSBootID: bootID, IsBackground: owners.IsBackground, Current: current},
	})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	run.stop = func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = run.manager.Shutdown(ctx)
			_ = owners.Close()
			_ = registry.Close()
			close(runtimeDone)
		})
	}
	t.Cleanup(run.stop)
	return run
}

// dispatchLocal dispatches a preview owned by a local lease, handed to the runtime when background.
func (r *runtimeRun) dispatchLocal(t *testing.T, now time.Time, background bool) DispatchRequest {
	t.Helper()
	request := testDispatchRequest(t, now)
	deadline := now.Add(30 * time.Minute)
	lease := OwnerSessionLeaseRequest{Target: request.Target}
	if background {
		lease.Background, lease.ExpiresAt = true, &deadline
	}
	local, err := r.owners.Acquire(lease, "local_acquire")
	if err != nil {
		t.Fatal(err)
	}
	request.OwnerSessionID, request.OwnerSessionKind = local.OwnerSessionID, OwnerSessionLocalLease
	request.UserDeadline, request.LeaseDeadline = &deadline, deadline
	if request.RequestHash, err = request.ComputeRequestHash(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.manager.Dispatch(context.Background(), testDispatchAuthorization(request, now), request); err != nil {
		t.Fatal(err)
	}
	return request
}

func (r *runtimeRun) stoppedLeases() int {
	r.leases.mu.Lock()
	defer r.leases.mu.Unlock()
	return len(r.leases.stopLeases)
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); !done(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func serverLease(request DispatchRequest, generation int64, state string) func(context.Context, Lease) (Lease, error) {
	return func(_ context.Context, stored Lease) (Lease, error) {
		lease := stored
		lease.Generation, lease.ETag, lease.State = generation, formatLeaseETag(stored.ID, generation), state
		lease.AllocationState, lease.EdgeState, lease.OriginState = "ready", "ready", "ready"
		return lease, nil
	}
}

func TestBackgroundPreviewResumesWithTheSameLeaseAfterAHostRuntimeRestart(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewBackgroundStore(filepath.Join(t.TempDir(), "preview", "background.json"))
	first := startRuntimeRun(t, store, "boot_1", now, nil)
	request := first.dispatchLocal(t, now, true)
	<-first.resolver.started

	first.stop()
	if first.stoppedLeases() != 0 {
		t.Fatalf("shutdown stopped the background lease %d times", first.stoppedLeases())
	}
	records, err := store.Load()
	if err != nil || len(records) != 1 || records[0].Request.PreviewID != request.PreviewID {
		t.Fatalf("records=%+v err=%v", records, err)
	}

	second := startRuntimeRun(t, store, "boot_1", now, serverLease(request, 4, "ready"))
	second.manager.RestoreBackground(context.Background())
	<-second.resolver.started
	second.resolver.mu.Lock()
	resumed := second.resolver.requests[0]
	second.resolver.mu.Unlock()
	if resumed.PreviewID != request.PreviewID || resumed.OperationID != request.OperationID || resumed.Endpoint != request.Endpoint || resumed.ExpectedGeneration != 4 {
		t.Fatalf("resumed %+v from %+v", resumed, request)
	}
	if second.manager.config.Sessions.Count() != 1 {
		t.Fatalf("sessions=%d", second.manager.config.Sessions.Count())
	}

	// An explicit stop ends the lease and forgets the record, so the next start does not resume it.
	session := second.manager.config.Sessions
	session.mu.RLock()
	var key SessionKey
	for k := range session.sessions {
		key = k
	}
	session.mu.RUnlock()
	if err := session.Revoke(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "record removal", func() bool { records, _ := store.Load(); return len(records) == 0 })
	if second.stoppedLeases() != 1 {
		t.Fatalf("stop calls=%d", second.stoppedLeases())
	}
}

func TestForegroundPreviewStillStopsItsLeaseWhenTheHostRuntimeShutsDown(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewBackgroundStore(filepath.Join(t.TempDir(), "background.json"))
	run := startRuntimeRun(t, store, "boot_1", now, nil)
	run.dispatchLocal(t, now, false)
	<-run.resolver.started
	run.stop()
	if run.stoppedLeases() != 1 {
		t.Fatalf("foreground lease stop calls=%d", run.stoppedLeases())
	}
	if records, _ := store.Load(); len(records) != 0 {
		t.Fatalf("foreground preview was recorded: %+v", records)
	}
}

func TestBackgroundPreviewIsNotResumedAfterAReboot(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewBackgroundStore(filepath.Join(t.TempDir(), "background.json"))
	first := startRuntimeRun(t, store, "boot_1", now, nil)
	request := first.dispatchLocal(t, now, true)
	<-first.resolver.started
	first.stop()

	second := startRuntimeRun(t, store, "boot_2", now, serverLease(request, 4, "ready"))
	second.manager.RestoreBackground(context.Background())
	if second.manager.config.Sessions.Count() != 0 {
		t.Fatal("a preview from the previous boot was resumed")
	}
	if records, _ := store.Load(); len(records) != 0 {
		t.Fatalf("stale record kept: %+v", records)
	}
}

func TestBackgroundPreviewThatEndedOnTheServerIsDroppedOnRestore(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewBackgroundStore(filepath.Join(t.TempDir(), "background.json"))
	first := startRuntimeRun(t, store, "boot_1", now, nil)
	first.dispatchLocal(t, now, true)
	<-first.resolver.started
	first.stop()

	gone := func(_ context.Context, stored Lease) (Lease, error) {
		return Lease{}, errors.Join(ErrDispatchInvalid, errors.New("preview not found"))
	}
	second := startRuntimeRun(t, store, "boot_1", now, gone)
	second.manager.RestoreBackground(context.Background())
	if second.manager.config.Sessions.Count() != 0 {
		t.Fatal("an ended preview was resumed")
	}
	if records, _ := store.Load(); len(records) != 0 {
		t.Fatalf("ended preview kept: %+v", records)
	}
}

func TestBackgroundStoreRoundTripsAndIgnoresAnUnreadableFile(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", "background.json")
	store := NewBackgroundStore(path)
	if records, err := store.Load(); err != nil || len(records) != 0 {
		t.Fatalf("missing file: %+v %v", records, err)
	}
	request := testDispatchRequest(t, now)
	if err := store.Save(BackgroundRecord{OSBootID: "boot_1", Request: request}); err != nil {
		t.Fatal(err)
	}
	if records, err := store.Load(); err != nil || len(records) != 1 || records[0].Request.RequestHash != request.RequestHash {
		t.Fatalf("round trip: %+v %v", records, err)
	}
	if err := store.Remove(request.OperationID); err != nil {
		t.Fatal(err)
	}
	if records, err := store.Load(); err != nil || len(records) != 0 {
		t.Fatalf("after remove: %+v %v", records, err)
	}
}
