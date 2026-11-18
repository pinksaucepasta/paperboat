package posthogcapture

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func testStore(t *testing.T) *db.DB {
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

// The tailer maps committed audit rows to events once, for the right account, and never reads past rows that
// are younger than the commit lag or that did not succeed.
func TestPostgresTailMapsAuditRowsToEvents(t *testing.T) {
	store, ctx := testStore(t), context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	var start int64
	if err := store.Pool().QueryRow(ctx, `SELECT coalesce(max(cursor_sequence),0) FROM audit_events`).Scan(&start); err != nil {
		t.Fatal(err)
	}
	// audit_events.actor_user_id names a real account.
	for _, id := range []string{"acct_" + x, "operator_" + x} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, canonical_email, email_verified, display_name, status, created_at, updated_at)
 VALUES ($1, $1, $1||'@example.test', $1||'@example.test', true, '', 'active', now(), now())`, id); err != nil {
			t.Fatal(err)
		}
	}
	// audit_events is append-only (no UPDATE), so each row is inserted with the age it should have.
	write := func(id, actor, actorType, kind, resourceID, result, metadata, age string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.audit_events (id, actor_user_id, actor_type, event_type, resource_type, resource_id, metadata, created_at, result)
 VALUES ($1, $2, $3, $4, 'test', $5, $6::jsonb, now() - $7::interval, $8)`, id+"_"+x, actor, actorType, kind, resourceID, metadata, age, result); err != nil {
			t.Fatal(err)
		}
	}
	old := "5 minutes"
	write("login", "acct_"+x, "user", "auth.login", "acct_"+x, "success", `{}`, old)
	write("grant", "operator_"+x, "admin", "operator.plan_grant", "personal:owner_"+x, "success", `{"product_code":"pro","state":"granted"}`, old)
	write("team", "operator_"+x, "admin", "operator.plan_revoke", "team:missing_"+x, "success", `{}`, old)
	write("logout", "acct_"+x, "user", "auth.logout", "acct_"+x, "success", `{}`, old)
	write("denied", "acct_"+x, "user", "auth.login", "acct_"+x, "denied", `{}`, old)
	write("young", "acct_"+x, "user", "auth.login", "acct_"+x, "success", `{}`, "0 seconds")

	c := New(Config{Host: "https://posthog.example.test", Key: "k"})
	types := make([]string, 0, len(auditEvents))
	for t := range auditEvents {
		types = append(types, t)
	}
	cursor, err := tailOnce(ctx, c, store.Pool(), types, start)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Event{}
	for len(c.queue) > 0 {
		e := <-c.queue
		got[e.UUID] = e
	}
	if len(got) != 2 {
		t.Fatalf("want the login and the plan grant only, got %d: %+v", len(got), got)
	}
	var login, plan Event
	for _, e := range got {
		if e.Name == "signed_in" {
			login = e
		} else {
			plan = e
		}
	}
	if login.Distinct != "acct_"+x || plan.Name != "plan_changed" || plan.Distinct != "owner_"+x || plan.Props["plan"] != "pro" || plan.Props["state"] != "granted" {
		t.Fatalf("login=%+v plan=%+v (the plan belongs to the workspace owner, not the operator)", login, plan)
	}
	if again, err := tailOnce(ctx, c, store.Pool(), types, cursor); err != nil || again < cursor || len(c.queue) != 0 {
		t.Fatalf("the cursor must not replay rows: cursor %d -> %d, queued %d, err %v", cursor, again, len(c.queue), err)
	}
}
