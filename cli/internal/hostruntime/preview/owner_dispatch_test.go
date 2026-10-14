package preview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLocalOwnerDispatchReleaseRetirementAndNewOperation(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	runtimeDone := make(chan struct{})
	defer close(runtimeDone)
	registry, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{AccountID: "account_1", MachineID: "machine_1", RuntimeDone: runtimeDone})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	leases, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_1", ControlToken: "control_secret", Registry: registry, TTL: 10 * time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer leases.Close()
	request := testDispatchRequest(t, now)
	local, err := leases.Acquire(OwnerSessionLeaseRequest{Target: request.Target}, "local_acquire")
	if err != nil {
		t.Fatal(err)
	}
	suffix, ok := strings.CutPrefix(local.OwnerSessionID, "session_")
	id, parseErr := uuid.Parse(suffix)
	if !ok || parseErr != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != suffix {
		t.Fatalf("invalid generated session ID %q", local.OwnerSessionID)
	}
	request.OwnerSessionID, request.OwnerSessionKind = local.OwnerSessionID, OwnerSessionLocalLease
	request.RequestHash, err = request.ComputeRequestHash()
	if err != nil {
		t.Fatal(err)
	}
	started, exited := make(chan struct{}), make(chan struct{})
	carrier := &sessionCarrier{run: func(ctx context.Context, _ Lease, _ func(Lease) error) error {
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}}
	manager, err := NewDispatchManager(DispatchManagerConfig{MachineID: "machine_1", Owners: registry, Leases: &sessionLeaseClient{}, Carriers: &dispatchResolver{carrier: carrier}, Readiness: &dispatchObserver{}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	auth := testDispatchAuthorization(request, now)
	mismatched := auth
	mismatched.OwnerSessionKind = OwnerSessionForeground
	if _, err := manager.Dispatch(context.Background(), mismatched, request); !errors.Is(err, ErrDispatchInvalid) {
		t.Fatalf("signed kind mismatch: %v", err)
	}
	for name, mutate := range map[string]func(*DispatchRequest){
		"kind":    func(r *DispatchRequest) { r.OwnerSessionKind = OwnerSessionForeground },
		"account": func(r *DispatchRequest) { r.AccountID = "account_other" },
		"target":  func(r *DispatchRequest) { r.Target.Address = "127.0.0.1:4000" },
	} {
		candidate := request
		candidate.OperationID = "operation_wrong_" + name
		mutate(&candidate)
		candidate.RequestHash, err = candidate.ComputeRequestHash()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Dispatch(context.Background(), testDispatchAuthorization(candidate, now), candidate); !errors.Is(err, ErrDispatchInvalid) {
			t.Fatalf("wrong %s admission: %v", name, err)
		}
		registry.mu.Lock()
		refs := registry.unbound[local.OwnerSessionID].refs
		registry.mu.Unlock()
		if refs != 1 {
			t.Fatalf("wrong %s admission changed lease references to %d", name, refs)
		}
	}
	if _, err := manager.Dispatch(context.Background(), auth, request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("local preview did not start")
	}
	if _, err := manager.Dispatch(context.Background(), auth, request); err != nil {
		t.Fatalf("same operation replay: %v", err)
	}
	if err := leases.Release(local.ID, local.Token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("released local preview did not stop")
	}
	// Wait for dispatch cleanup to release its reference before retirement.
	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		attached := registry.unbound[local.OwnerSessionID] != nil && registry.unbound[local.OwnerSessionID].refs > 1
		registry.mu.Unlock()
		if !attached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatch owner reference leaked")
		}
		time.Sleep(time.Millisecond)
	}
	now = now.Add(21 * time.Second)
	leases.Sweep(now)
	registry.mu.Lock()
	remaining := len(registry.unbound) + len(registry.sessions)
	registry.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("retired registry entries=%d", remaining)
	}
	request.OperationID = "operation_new_dispatch"
	request.RequestHash, err = request.ComputeRequestHash()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Dispatch(context.Background(), testDispatchAuthorization(request, now), request); !errors.Is(err, ErrDispatchInvalid) {
		t.Fatalf("released owner accepted for new operation: %v", err)
	}
	carrier.mu.Lock()
	runs := carrier.runs
	carrier.mu.Unlock()
	if runs != 1 {
		t.Fatalf("carrier starts=%d", runs)
	}
}
