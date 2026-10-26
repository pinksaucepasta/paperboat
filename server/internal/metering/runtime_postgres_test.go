package metering

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// An idle machine heartbeats every few seconds. Before, each heartbeat rewrote its user_machines row two or three
// times (presence, diagnostics), ~2,600 row versions an hour on staging (PLAN 3b, Lane W). An unchanged heartbeat
// now leaves the row untouched (same xmin); last_seen_at is refreshed once per presence interval without a version
// bump; a real change (offline to online, new diagnostics) still writes and bumps the version.
func TestUnchangedHeartbeatWritesNoMachineRowOnPostgres(t *testing.T) {
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
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, machine, env := "hb_user_"+s, "hb_machine_"+s, "hb_env_"+s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
VALUES($1,$2,$3,'hb-'||$4,'linux','amd64','/w','offline','occupied',false)`, machine, user, env, s[len(s)-6:]); err != nil {
		t.Fatal(err)
	}
	type row struct {
		xmin     string
		version  int64
		lastSeen time.Time
	}
	read := func() row {
		t.Helper()
		var r row
		if err := store.SQL().QueryRowContext(ctx, `SELECT xmin::text, version, last_seen_at FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&r.xmin, &r.version, &r.lastSeen); err != nil {
			t.Fatal(err)
		}
		return r
	}
	boot := "boot-" + s
	heartbeat := func(repo *RuntimeRepository, generation uint64) {
		t.Helper()
		now := time.Now().UTC()
		if err := repo.RecordRuntimeObservation(ctx, RuntimeObservation{EnvironmentID: env, MachineID: machine, ObservedAt: now, WorkerGeneration: generation, OSBootID: boot, WorkerServiceScope: "user", ConnectorState: "ready", DiagnosticsObservedAt: now, Capabilities: []string{"terminal_host"}}); err != nil {
			t.Fatal(err)
		}
	}
	idle := NewRuntimeRepository(store, time.Hour)

	heartbeat(idle, 1) // offline to online: a real change
	online := read()
	for range 5 {
		heartbeat(idle, 1)
	}
	if after := read(); after.xmin != online.xmin {
		t.Fatalf("5 unchanged heartbeats rewrote the machine row (xmin %s -> %s)", online.xmin, after.xmin)
	}

	heartbeat(idle, 2) // a new worker generation is a real diagnostics change
	if after := read(); after.xmin == online.xmin {
		t.Fatal("a new worker generation was not written")
	}
	// Unchanged diagnostics are not rewritten even when the presence interval refreshes last_seen_at.
	var diagnostics time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT runtime_diagnostics_observed_at FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&diagnostics); err != nil {
		t.Fatal(err)
	}

	// Once last_seen_at is older than the presence interval, one heartbeat refreshes it, and that refresh is not a
	// change clients see: the version stays.
	stale := NewRuntimeRepository(store, time.Nanosecond)
	before := read()
	time.Sleep(10 * time.Millisecond)
	heartbeat(stale, 2)
	refreshed := read()
	if !refreshed.lastSeen.After(before.lastSeen) {
		t.Fatalf("last_seen_at not refreshed after the presence interval: %v -> %v", before.lastSeen, refreshed.lastSeen)
	}
	if refreshed.version != before.version {
		t.Fatalf("a last_seen_at-only refresh bumped the version %d -> %d", before.version, refreshed.version)
	}
	var diagnosticsAfter time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT runtime_diagnostics_observed_at FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&diagnosticsAfter); err != nil || !diagnosticsAfter.Equal(diagnostics) {
		t.Fatalf("unchanged diagnostics were rewritten: %v -> %v (%v)", diagnostics, diagnosticsAfter, err)
	}
}
