package selfhost

import (
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A self-host installation is ready only while its node's heartbeat is fresh.
// A stopped or uninstalled node keeps its stored ready flag until the registry
// row is deleted, so every listing must derive readiness from the heartbeat.
func TestInstallationReadyFollowsHeartbeatFreshnessOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	key := "ready-freshness-test-encryption-key-32"
	service := New(store, key)
	account := "ready_" + randomIdentity()
	installed := scopedInstallationFixture(t, store, key, account, "relay")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,process_epoch='ready_epoch',last_heartbeat_at=now() WHERE id=$1`, installed.NodeID)
	readyNow := func(stage string) bool {
		t.Helper()
		listed, err := service.ListScoped(ctx, account, Scope{}, false)
		if err != nil {
			t.Fatal(err)
		}
		var fromList *Installation
		for i := range listed {
			if listed[i].InstallationID == installed.InstallationID {
				fromList = &listed[i]
			}
		}
		if fromList == nil {
			t.Fatalf("%s: installation missing from listing", stage)
		}
		inventory, err := service.ListScopedInventory(ctx, account, Scope{}, false, InventoryOptions{Q: installed.InstallationID})
		if err != nil || len(inventory.Installations) != 1 {
			t.Fatalf("%s: inventory %+v %v", stage, inventory, err)
		}
		detail, err := service.GetInstallation(ctx, account, installed.InstallationID, false)
		if err != nil {
			t.Fatal(err)
		}
		if fromList.Ready != inventory.Installations[0].Ready || fromList.Ready != detail.Ready {
			t.Fatalf("%s: listing %v, inventory %v and detail %v disagree", stage, fromList.Ready, inventory.Installations[0].Ready, detail.Ready)
		}
		return fromList.Ready
	}
	if !readyNow("fresh heartbeat") {
		t.Fatal("installation with a fresh heartbeat is not ready")
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1`, installed.NodeID)
	if readyNow("stale heartbeat") {
		t.Fatal("uninstalled node with a stale heartbeat still reports ready")
	}
}
