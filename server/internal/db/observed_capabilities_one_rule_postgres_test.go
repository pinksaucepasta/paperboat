package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// The runtime observation (every heartbeat) and the capability-apply report both write observed
// capabilities. They used to write different sets, which flipped the column and stormed the inventory
// topic (prod 2026-10-23, 23:12Z). With one rule, alternating writes leave the value and the topic still,
// and ssh_host comes from the applied selection.
func TestObservedCapabilitiesHaveOneRuleForBothWritersOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, machine, env := "oc_user_"+s, "oc_machine_"+s, "oc_env_"+s
	for _, q := range []string{
		`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`,
	} {
		if _, err := store.SQL().ExecContext(ctx, q, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,capabilities_desired_version,last_seen_at)
VALUES($1,$2,$3,'oc-'||$4,'linux','amd64','/w','online','occupied',true,ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host'],1,now())`, machine, user, env, s[len(s)-6:]); err != nil {
		t.Fatal(err)
	}
	q := store.Queries()
	heartbeat := func() {
		t.Helper()
		if _, err := q.MarkUserMachineOnlineFromHelper(ctx, dbsqlc.MarkUserMachineOnlineFromHelperParams{ID: machine, EnvironmentID: env, ObservedCapabilities: []string{"file_receive", "preview_launch", "terminal_host", "session_host", "keep_awake"}}); err != nil {
			t.Fatal(err)
		}
	}
	applied := func() {
		t.Helper()
		if rows, err := q.RecordUserMachineCapabilitiesObservation(ctx, dbsqlc.RecordUserMachineCapabilitiesObservationParams{AppliedCapabilities: []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host"}, ObservedVersion: 1, Status: "applied", ID: machine, EnvironmentID: env}); err != nil || rows != 1 {
			t.Fatalf("apply report rows=%d err=%v", rows, err)
		}
	}
	observed := func() (caps string) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT array_to_string(observed_capabilities, ',') FROM paperboat.user_machines WHERE id=$1`, machine).Scan(&caps); err != nil {
			t.Fatal(err)
		}
		return caps
	}
	generation := func() int64 {
		t.Helper()
		var g int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='inventory'),0)`, user).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	heartbeat()
	applied() // ssh_host is now applied: one real change
	want := "file_receive,keep_awake,preview_launch,session_host,ssh_host,terminal_host"
	if got := observed(); got != want {
		t.Fatalf("observed after apply = %v, want %v", got, want)
	}
	before := generation()
	for range 5 {
		heartbeat()
		applied()
	}
	if got := observed(); got != want {
		t.Fatalf("observed after alternating writers = %v, want %v", got, want)
	}
	if after := generation(); after != before {
		t.Fatalf("alternating writers moved the inventory topic %d -> %d", before, after)
	}
}
