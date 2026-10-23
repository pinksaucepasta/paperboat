//go:build darwin || linux

package localdaemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// While the latest network refresh withheld relays at the plan limit, the daemon snapshot names it;
// the wrapper still exposes the inventory's refresh to the local API (pb login refreshes through it).
func TestSnapshotNamesTheRelayPlanLimit(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{}}}}
	inventory, _ := newTestInventory(t, source, func() time.Time { return now })
	var denial *api.APIError
	var wrapped localapi.SnapshotSource = relayDenialSource{Inventory: inventory, denial: func() *api.APIError { return denial }}
	if _, ok := wrapped.(interface{ Refresh(context.Context) error }); !ok {
		t.Fatal("the wrapper hides the inventory refresh")
	}
	if err := inventory.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := wrapped.Snapshot(context.Background())
	if err != nil || len(snapshot.Health) != 0 {
		t.Fatalf("without a denial: health=%+v err=%v", snapshot.Health, err)
	}
	denial = &api.APIError{Status: http.StatusConflict, Code: localapi.RelayQuotaCode, Message: "Hosted relay bandwidth is used up."}
	snapshot, err = wrapped.Snapshot(context.Background())
	if err != nil || len(snapshot.Health) != 1 || snapshot.Health[0].Code != localapi.RelayQuotaCode || snapshot.Health[0].Title != denial.Message || snapshot.Validate() != nil {
		t.Fatalf("with a denial: health=%+v err=%v", snapshot.Health, err)
	}
}
