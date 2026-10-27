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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if inventory.SignedOut(ctx) {
		t.Fatal("canceled stream request was reported signed out")
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil || snapshot.DaemonState != "ready" || len(snapshot.Health) != 0 || len(snapshot.Machines) != 1 {
		t.Fatalf("canceled stream poisoned inventory health: snapshot=%+v err=%v", snapshot, err)
	}
	if !inventory.SignedOut(ctx) {
		t.Fatal("canceled caller hid a current authentication rejection")
	}
}
