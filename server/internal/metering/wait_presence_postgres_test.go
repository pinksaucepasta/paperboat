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

// The held machine-events wait is an idle machine's presence: it refreshes last_seen_at of an online machine once the
// row is older than the gate and writes nothing else (same version), it writes nothing while the row is fresh, and it
// never brings an offline machine online (that takes an observation, which also restores the capabilities).
func TestMachineWaitRefreshesPresenceOnlyOfAnOnlineMachineOnPostgres(t *testing.T) {
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
	user, machine, env := "wp_user_"+s, "wp_machine_"+s, "wp_env_"+s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,last_seen_at)
VALUES($1,$2,$3,'wp-'||$4,'linux','amd64','/w','offline','occupied',false,now()-interval '1 hour')`, machine, user, env, s[len(s)-6:]); err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeRepository(store, time.Hour)
	type row struct {
		state    string
		version  int64
		lastSeen time.Time
	}
	read := func() row {
		t.Helper()
		var r row
		if err := store.SQL().QueryRowContext(ctx, `SELECT state, version, last_seen_at FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&r.state, &r.version, &r.lastSeen); err != nil {
			t.Fatal(err)
		}
		return r
	}
	touch := func() {
		t.Helper()
		if err := repo.TouchMachinePresence(ctx, machine); err != nil {
			t.Fatal(err)
		}
	}

	touch() // an offline machine stays offline: only an observation brings it online
	if got := read(); got.state != "offline" || time.Since(got.lastSeen) < 30*time.Minute {
		t.Fatalf("a wait changed an offline machine: %+v", got)
	}

	if err := repo.RecordRuntimeObservation(ctx, RuntimeObservation{EnvironmentID: env, MachineID: machine, ObservedAt: time.Now().UTC(), Capabilities: []string{"terminal_host"}}); err != nil {
		t.Fatal(err)
	}
	fresh := read()
	if fresh.state != "online" {
		t.Fatalf("the observation did not bring the machine online: %+v", fresh)
	}
	touch() // fresh: inside the gate nothing is written
	if got := read(); !got.lastSeen.Equal(fresh.lastSeen) {
		t.Fatalf("a wait inside the gate rewrote last_seen_at: %v -> %v", fresh.lastSeen, got.lastSeen)
	}

	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET last_seen_at = now() - interval '45 seconds' WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	stale := read()
	touch() // older than the gate: refreshed, and nothing a client reads changes
	got := read()
	if !got.lastSeen.After(stale.lastSeen) || got.version != stale.version || got.state != "online" {
		t.Fatalf("a wait past the gate did not refresh presence alone: %+v -> %+v", stale, got)
	}
}
