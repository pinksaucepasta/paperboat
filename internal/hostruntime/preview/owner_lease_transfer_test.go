package preview

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOwnerSessionLeaseTransferPreservesLiveDispatch(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	registry, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{MachineID: "machine_01", RuntimeDone: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	manager, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: "control_secret", Registry: registry, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	target := LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}
	lease, err := manager.Acquire(OwnerSessionLeaseRequest{Target: target}, "transfer_01")
	if err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(time.Hour)
	if _, err := manager.TransferBackground(lease.ID, lease.Token, deadline); !errors.Is(err, ErrOwnerSessionLeaseConflict) {
		t.Fatalf("unattached transfer: %v", err)
	}
	done, err := registry.OwnerSessionDoneForTarget("account_01", "machine_01", lease.OwnerSessionID, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.TransferBackground(lease.ID, "wrong", deadline); !errors.Is(err, ErrOwnerSessionLeaseUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	for _, invalid := range []time.Time{now, now.Add(BackgroundPreviewMaximumTTL + time.Second)} {
		if _, err := manager.TransferBackground(lease.ID, lease.Token, invalid); !errors.Is(err, ErrOwnerSessionLeaseInvalid) {
			t.Fatalf("invalid deadline: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		updated, err := manager.TransferBackground(lease.ID, lease.Token, deadline)
		if err != nil {
			t.Fatal(err)
		}
		if updated.ID != lease.ID || updated.OwnerSessionID != lease.OwnerSessionID || updated.Token != lease.Token || updated.Target != target || !updated.Background || !updated.ExpiresAt.Equal(deadline) {
			t.Fatal("transfer changed identity or omitted deadline")
		}
	}
	if _, err := manager.TransferBackground(lease.ID, lease.Token, deadline.Add(time.Minute)); !errors.Is(err, ErrOwnerSessionLeaseConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	manager.Sweep(now.Add(5 * time.Minute))
	select {
	case <-done:
		t.Fatal("transfer lost active dispatch")
	default:
	}
	manager.Sweep(deadline)
	select {
	case <-done:
	default:
		t.Fatal("background did not expire")
	}
	if _, err := manager.TransferBackground(lease.ID, lease.Token, deadline.Add(time.Hour)); !errors.Is(err, ErrOwnerSessionLeaseLost) {
		t.Fatalf("revival: %v", err)
	}
}

type transferLostResponseTransport struct {
	base    http.RoundTripper
	dropped bool
}

func (r *transferLostResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(req)
	if err == nil && req.Method == http.MethodPatch && !r.dropped {
		r.dropped = true
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, errors.New("response lost after commit")
	}
	return response, err
}
func TestOwnerSessionLeaseTransferClientReconcilesLostResponse(t *testing.T) {
	registry, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{MachineID: "machine_01", RuntimeDone: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	manager, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: "control_secret", Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	server := httptest.NewServer(manager)
	defer server.Close()
	transport := &transferLostResponseTransport{base: http.DefaultTransport}
	client, err := NewLocalOwnerSessionClient(server.URL, "control_secret", &http.Client{Transport: transport, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := client.Acquire(context.Background(), "", LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := registry.OwnerSessionDoneForTarget("account_01", "machine_01", lease.OwnerSessionID, lease.Target)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(time.Hour)
	updated, err := client.TransferBackground(context.Background(), lease, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if !transport.dropped || updated.ID != lease.ID || !updated.Background {
		t.Fatal("lost response was not safely replayed")
	}
	if err := client.Release(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("transferred lease cannot be stopped")
	}
}
