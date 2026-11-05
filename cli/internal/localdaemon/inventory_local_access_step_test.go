package localdaemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type stepFailure struct{ step string }

func (e stepFailure) Error() string           { return "local access step failed" }
func (e stepFailure) LocalAccessStep() string { return e.step }

// pb doctor names the local access step that fails: a step still being retried while native access works keeps
// the daemon ready with a warning, and a failed reconciliation names its step in the error title.
func TestInventoryNamesTheFailingLocalAccessStep(t *testing.T) {
	for _, c := range []struct {
		name, state, code, severity, step string
		err                               error
	}{
		{"pending", "ready", "browser_urls_unavailable", "warning", "browser certificate", LocalAccessPendingError{Step: "browser_certificate"}},
		{"failed", "degraded", "local_access_unavailable", "error", "machine names", stepFailure{step: "machine_names"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, err := localapi.NewSnapshotStore(nil)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
			inventory, err := NewInventory(InventoryConfig{
				Source:            &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
				Store:             store,
				RefreshInterval:   time.Second,
				RequestTimeout:    time.Second,
				Clock:             func() time.Time { return now },
				ReconcileMachines: func(context.Context, []api.UserMachine) error { return c.err },
			})
			if err != nil {
				t.Fatal(err)
			}
			inventory.setupSince = time.Unix(1, 0) // past start-up: a failure is degraded
			refreshErr := inventory.Refresh(context.Background())
			if c.state == "ready" && refreshErr != nil {
				t.Fatalf("a pending step must not fail the refresh: %v", refreshErr)
			}
			snapshot, err := store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.DaemonState != c.state || len(snapshot.Health) != 1 {
				t.Fatalf("snapshot state=%q health=%#v", snapshot.DaemonState, snapshot.Health)
			}
			health := snapshot.Health[0]
			if health.Code != c.code || health.Severity != c.severity || !strings.Contains(health.Title, c.step) {
				t.Fatalf("health=%#v; want %s/%s naming %q", health, c.code, c.severity, c.step)
			}
		})
	}
	if errors.Is(LocalAccessPendingError{Step: "x"}, context.Canceled) {
		t.Fatal("unexpected")
	}
}
