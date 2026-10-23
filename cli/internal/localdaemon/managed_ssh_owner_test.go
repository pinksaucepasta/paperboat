package localdaemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// Managed SSH used to start before the daemon served its local API; on a slow server its start (key
// registration, configuration, alias targets) kept the socket absent for 40 s+ and updates rolled back
// (2026-10-23). It now starts from the inventory refresh, a failed start is retried on the next refresh
// instead of staying off until a restart, the outcome is reported only when it changes, and a
// successful start asks for one more inventory refresh so machines show SSH ready.
func TestManagedSSHStartsFromTheRefreshAndRetriesAFailedStart(t *testing.T) {
	failure := errors.New("register managed SSH client key: server did not answer")
	starts, reports, refreshes := 0, 0, 0
	owner := &managedSSHOwner{
		start: func(context.Context, ManagedSSHConfig) (*ManagedSSHRuntime, error) {
			starts++
			if starts <= 2 {
				return nil, failure
			}
			return &ManagedSSHRuntime{refresh: func(context.Context) error { refreshes++; return nil }}, nil
		},
		report: func(context.Context, error) { reports++ },
	}
	requested := make(chan struct{}, 1)
	owner.onStarted = func() { requested <- struct{}{} }
	for range 2 {
		if err := owner.refresh(t.Context()); err != nil {
			t.Fatalf("failed start must not fail the refresh: %v", err)
		}
	}
	if starts != 2 || reports != 1 {
		t.Fatalf("after two failed starts: starts=%d reports=%d, want 2 starts and the failure reported once", starts, reports)
	}
	if err := owner.refresh(t.Context()); err != nil || starts != 3 || reports != 2 {
		t.Fatalf("recovered start: err=%v starts=%d reports=%d", err, starts, reports)
	}
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("a successful start did not ask for an inventory refresh")
	}
	if err := owner.refresh(t.Context()); err != nil || starts != 3 || refreshes != 1 {
		t.Fatalf("running managed SSH: err=%v starts=%d refreshes=%d, want a configuration refresh and no new start", err, starts, refreshes)
	}
}

func TestInventoryRefreshesWhenAsked(t *testing.T) {
	source := &scriptedMachineSource{results: []machineResult{{}}}
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan struct{}, 4)
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, RefreshInterval: time.Hour, OnRefresh: func(error) { refreshed <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = inventory.runTicker(ctx) }()
	inventory.RequestRefresh()
	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestRefresh did not refresh the inventory")
	}
}
