package preview

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type leaseWorker struct {
	registry *RuntimeOwnerSessionRegistry
	manager  *OwnerSessionLeaseManager
	server   *httptest.Server
	token    string
}

func startLeaseWorker(t *testing.T, token string) *leaseWorker {
	t.Helper()
	registry, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{MachineID: "machine_01", RuntimeDone: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: token, Registry: registry, TTL: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	w := &leaseWorker{registry: registry, manager: manager, server: httptest.NewServer(manager), token: token}
	t.Cleanup(func() { w.stop() })
	return w
}

func (w *leaseWorker) stop() {
	w.server.Close()
	_ = w.manager.Close()
	_ = w.registry.Close()
}

func (w *leaseWorker) client(t *testing.T) *LocalOwnerSessionClient {
	t.Helper()
	c, err := NewLocalOwnerSessionClient(w.server.URL, w.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pbvm4: pb update replaced the worker holding a foreground `pb tunnel` /
// `pb preview` owner-session lease; the CLI's next heartbeat was refused and
// the tunnel exited. It must reattach the same owner session to the new worker
// (new endpoint and control token) and keep serving.
func TestForegroundOwnerLeaseReattachesAcrossWorkerRestart(t *testing.T) {
	target := LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}
	old := startLeaseWorker(t, "control_old")
	client := old.client(t)
	lease, err := client.Acquire(t.Context(), "", target)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := old
	connect := func() (*LocalOwnerSessionClient, error) {
		mu.Lock()
		defer mu.Unlock()
		return NewLocalOwnerSessionClient(current.server.URL, current.token, nil)
	}
	reattached := make(chan OwnerSessionLease, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- KeepAliveReattaching(ctx, client, lease, connect, func(_ *LocalOwnerSessionClient, l OwnerSessionLease) { reattached <- l })
	}()
	// The update stops the old worker and starts a new one with a new
	// endpoint and token; it knows nothing of the old lease.
	old.stop()
	fresh := startLeaseWorker(t, "control_new")
	mu.Lock()
	current = fresh
	mu.Unlock()
	var next OwnerSessionLease
	select {
	case next = <-reattached:
	case err := <-done:
		t.Fatal("foreground lease ended at worker restart:", err)
	case <-time.After(15 * time.Second):
		t.Fatal("foreground lease did not reattach to the restarted worker")
	}
	if next.OwnerSessionID != lease.OwnerSessionID || next.Target != target {
		t.Fatalf("reattached a different owner session: %+v", next)
	}
	if _, err := fresh.registry.OwnerSessionDoneForTarget("account_01", "machine_01", lease.OwnerSessionID, "local_lease", target); err != nil {
		t.Fatal("restarted worker cannot dispatch the preview's owner session:", err)
	}
	// A deliberate stop on the new worker still ends the CLI's lease, even
	// once the released entry has been swept and the worker no longer knows it.
	if err := fresh.client(t).Release(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	fresh.manager.Sweep(time.Now().Add(time.Hour))
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ErrOwnerSessionLeaseLost) && !errors.Is(err, ErrOwnerSessionLeaseUnauthorized) {
			t.Fatalf("released lease returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a deliberately released lease was resurrected")
	}
	if _, err := fresh.registry.OwnerSessionDoneForTarget("account_01", "machine_01", lease.OwnerSessionID, "local_lease", target); err == nil {
		t.Fatal("released owner session was registered again")
	}
}
