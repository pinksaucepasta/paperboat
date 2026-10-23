package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
)

// A presence flip (online <-> offline) must not bump the inventory topic of the machine's audience; a change
// of name, membership or lifecycle (first online, rename, seat, revoke) must.
func TestInventoryTopicIgnoresPresenceFlipsOnPostgres(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, flipping, peer := "it_user_"+s, "it_flip_"+s, "it_peer_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	for _, id := range []string{flipping, peer} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
VALUES($1,$2,$1,$3,'linux','amd64','/w','pending','occupied',false)`, id, user, "it-"+id[3:7]+fmt.Sprint(len(id)))
	}
	generation := func() int64 {
		t.Helper()
		var g int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(max(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='inventory'`, peer).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	step := func(name, update string, wantBump bool) {
		t.Helper()
		before := generation()
		exec(update, flipping)
		bumped := generation() != before
		if bumped != wantBump {
			t.Fatalf("%s: peer's inventory topic bumped=%v, want %v", name, bumped, wantBump)
		}
	}
	step("first online", `UPDATE paperboat.user_machines SET state='online',online=true WHERE id=$1`, true)
	step("goes offline", `UPDATE paperboat.user_machines SET state='offline',online=false WHERE id=$1`, false)
	step("back online", `UPDATE paperboat.user_machines SET state='online',online=true WHERE id=$1`, false)
	step("online flag only", `UPDATE paperboat.user_machines SET online=false WHERE id=$1`, false)
	step("rename", `UPDATE paperboat.user_machines SET alias='renamed-machine' WHERE id=$1`, true)
	step("seat released", `UPDATE paperboat.user_machines SET seat_state='released' WHERE id=$1`, true)
	step("revoked", `UPDATE paperboat.user_machines SET state='revoked',revoked_at=now() WHERE id=$1`, true)
}
