package localdaemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// A daemon restarted while Paperboat is unreachable started with no machines, so `pb exec <peer>` asked the server
// and failed after 30 s although the peer network had restored its peers (hp containers 2026-11-16). It now starts
// from the last answered list, the first answer replaces it (a removed machine does not come back), and signing out
// removes it.
func TestRestartWhilePaperboatIsUnreachableStartsFromTheLastAnsweredMachines(t *testing.T) {
	now := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)
	cache := filepath.Join(t.TempDir(), InventoryCacheFile)
	ready := api.UserMachine{ID: "machine_peer", Alias: "peer", EnvironmentID: "environment_1", State: "ready", Online: true, InstallationGeneration: 3}
	other := api.UserMachine{ID: "machine_other", Alias: "other", State: "ready", Online: true, InstallationGeneration: 1}
	start := func(results ...machineResult) *localapi.SnapshotStore {
		t.Helper()
		store, err := localapi.NewSnapshotStore(&localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "starting", DaemonVersion: buildinfo.Version})
		if err != nil {
			t.Fatal(err)
		}
		inventory, err := NewInventory(InventoryConfig{Source: &scriptedMachineSource{results: results}, Store: store, RefreshInterval: time.Second, RequestTimeout: time.Second, Clock: func() time.Time { return now }, CachePath: cache})
		if err != nil {
			t.Fatal(err)
		}
		inventory.restoreCachedMachines() // what Run does before its first refresh
		_ = inventory.Refresh(context.Background())
		return store
	}
	machines := func(store *localapi.SnapshotStore) map[string]localapi.MachineStatus {
		t.Helper()
		snapshot, err := store.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]localapi.MachineStatus{}
		for _, machine := range snapshot.Machines {
			found[machine.ID] = machine
		}
		return found
	}

	start(machineResult{machines: []api.UserMachine{ready, other}})
	if info, err := os.Stat(cache); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("answered list not cached owner-only: %v %v", info, err)
	}
	down := machineResult{err: apiDown("/v1/machines")}
	restarted := machines(start(down))
	if peer, ok := restarted["machine_peer"]; !ok || peer.RuntimeState != "ready" || !peer.Eligible || peer.Generation != 3 || peer.EnvironmentID != "environment_1" {
		t.Fatalf("restart with Paperboat unreachable lost the peer: %+v", restarted)
	}

	// Removed while up: the answer replaces the cache, so a later offline restart does not bring it back.
	start(machineResult{machines: []api.UserMachine{ready}})
	if _, ok := machines(start(down))["machine_other"]; ok {
		t.Fatal("a machine the server removed came back from the cache")
	}

	start(machineResult{err: api.ErrUnauthenticated})
	if _, err := os.Stat(cache); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("signing out kept the machine cache:", err)
	}
	if got := machines(start(down)); len(got) != 0 {
		t.Fatalf("machines after sign-out and an offline restart: %+v", got)
	}
}
