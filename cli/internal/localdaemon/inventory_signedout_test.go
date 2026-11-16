package localdaemon

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type cancelAwareMachineSource struct{ *scriptedMachineSource }

func (s cancelAwareMachineSource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	if err := ctx.Err(); err != nil {
		return nil, inventorySourceFailure("machine_list", err)
	}
	return s.scriptedMachineSource.ListUserMachines(ctx)
}

func TestSignedOutCanceledRequestStillChecksCurrentAuthentication(t *testing.T) {
	source := cancelAwareMachineSource{&scriptedMachineSource{results: []machineResult{
		{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}},
		{err: api.ErrUnauthenticated},
	}}}
	inventory, store := newTestInventory(t, source, time.Now)
	if err := inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if inventory.SignedOut(ctx) {
		t.Fatal("canceled stream request was reported signed out")
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil || snapshot.DaemonState != "ready" || len(snapshot.Health) != 0 || len(snapshot.Machines) != 1 {
		t.Fatalf("canceled stream poisoned inventory health: snapshot=%+v err=%v", snapshot, err)
	}
	// The inventory owner observes revocation; failed requests read that state.
	if err := inventory.Refresh(t.Context()); err == nil {
		t.Fatal("authentication rejection was hidden")
	}
	if !inventory.SignedOut(ctx) {
		t.Fatal("canceled caller hid a current authentication rejection")
	}
}

// A failed peer stream open reads the authentication state and never queues an inventory refresh: a client
// retrying an unreachable peer kept a daemon re-listing every machine back to back (2026-11-16).
func TestSignedOutQueuesNoInventoryRefresh(t *testing.T) {
	inventory, _ := newTestInventory(t, &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}, time.Now)
	for range 5 {
		if inventory.SignedOut(t.Context()) {
			t.Fatal("enrolled machine reported signed out")
		}
	}
	select {
	case <-inventory.wake:
		t.Fatal("a failed stream open queued an inventory refresh")
	default:
	}
}
