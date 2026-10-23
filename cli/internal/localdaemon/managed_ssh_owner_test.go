package localdaemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
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
		lifetime: t.Context(),
		start: func(context.Context, ManagedSSHConfig, []api.UserMachine) (*ManagedSSHRuntime, error) {
			starts++
			if starts <= 2 {
				return nil, failure
			}
			return &ManagedSSHRuntime{refresh: func(context.Context, []api.UserMachine) error { refreshes++; return nil }}, nil
		},
		report: func(context.Context, error) { reports++ },
	}
	requested := make(chan struct{}, 1)
	owner.onStarted = func() { requested <- struct{}{} }
	for range 2 {
		if err := owner.refresh(t.Context(), nil); err != nil {
			t.Fatalf("failed start must not fail the refresh: %v", err)
		}
	}
	if starts != 2 || reports != 1 {
		t.Fatalf("after two failed starts: starts=%d reports=%d, want 2 starts and the failure reported once", starts, reports)
	}
	if err := owner.refresh(t.Context(), nil); err != nil || starts != 3 || reports != 2 {
		t.Fatalf("recovered start: err=%v starts=%d reports=%d", err, starts, reports)
	}
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("a successful start did not ask for an inventory refresh")
	}
	if err := owner.refresh(t.Context(), nil); err != nil || starts != 3 || refreshes != 1 {
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
	go func() { _ = inventory.Run(ctx) }()
	select {
	case <-refreshed: // the first refresh
	case <-time.After(2 * time.Second):
		t.Fatal("no first refresh")
	}
	inventory.RequestRefresh()
	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestRefresh did not refresh the inventory")
	}
}

// Round 7 regression (dadape, 2026-10-23): the owner started managed SSH on the refresh's 15 s context,
// and the agent serves on the context it is started with, so it stopped as soon as that refresh ended:
// no agent socket, `pb ssh` denied. Managed SSH starts on the daemon's lifetime, whether the server
// answers quickly or the refresh's own context ends first (slow server), and stays up afterwards.
func TestManagedSSHRunsOnTheDaemonLifetimeNotTheRefresh(t *testing.T) {
	for name, refreshBudget := range map[string]time.Duration{"healthy server": time.Minute, "slow server": time.Millisecond} {
		t.Run(name, func(t *testing.T) {
			lifetime, stopDaemon := context.WithCancel(t.Context())
			defer stopDaemon()
			var agentCtx context.Context
			owner := &managedSSHOwner{
				lifetime: lifetime,
				start: func(ctx context.Context, _ ManagedSSHConfig, _ []api.UserMachine) (*ManagedSSHRuntime, error) {
					time.Sleep(20 * time.Millisecond) // the server takes longer than the slow refresh's budget
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					agentCtx = ctx
					return &ManagedSSHRuntime{refresh: func(context.Context, []api.UserMachine) error { return nil }}, nil
				},
				report: func(context.Context, error) {},
			}
			refreshCtx, endRefresh := context.WithTimeout(t.Context(), refreshBudget)
			_ = owner.refresh(refreshCtx, nil)
			endRefresh()
			if agentCtx == nil {
				t.Fatal("managed SSH did not start")
			}
			if agentCtx.Err() != nil {
				t.Fatal("the agent's context ended with the refresh; managed SSH must run on the daemon lifetime")
			}
			stopDaemon()
			if agentCtx.Err() == nil {
				t.Fatal("the agent's context must end with the daemon")
			}
		})
	}
}
