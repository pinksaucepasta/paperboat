package activity

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func openTestStore(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	return store
}

// Retention is per workspace plan, and nothing but the prune job can delete an event.
func TestPostgresAuditRetentionPerPlan(t *testing.T) {
	store := openTestStore(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	free, pro, team := "ret_free_"+suffix, "ret_pro_"+suffix, "ret-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, free, pro)
	exec(`INSERT INTO billing_workspaces(id,user_id,state,quantity,period_start,period_end) VALUES('personal:'||$1,$1,'active',1,now()-interval '1 day',now()+interval '29 days')`, pro)
	exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Retention team')`, team, free)
	event := func(id, kind, resource string, age string) {
		exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata,created_at) VALUES($1,$2,'user','test.event',$3,$4,'{}',now()-$5::interval)`, id+suffix, free, kind, resource, age)
	}
	event("free_old_", "user", free, "40 days")
	event("free_new_", "user", free, "10 days")
	event("pro_mid_", "user", pro, "40 days")
	event("pro_old_", "user", pro, "100 days")
	event("team_mid_", "team", team, "100 days")
	event("team_old_", "team", team, "400 days")
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM audit_events WHERE id=$1`, "free_old_"+suffix); err == nil {
		t.Fatal("an event was deleted outside the prune job")
	}
	s := New(store, config.Default().Business)
	for range 20 {
		n, err := s.Prune(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	want := map[string]bool{"free_old_": false, "free_new_": true, "pro_mid_": true, "pro_old_": false, "team_mid_": true, "team_old_": false}
	for prefix, kept := range want {
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE id=$1`, prefix+suffix).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 1) != kept {
			t.Errorf("%s kept=%v, want %v", prefix, n == 1, kept)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE audit_events SET metadata='{}' WHERE id=$1`, "free_new_"+suffix); err == nil {
		t.Fatal("an event was updated")
	}
}
