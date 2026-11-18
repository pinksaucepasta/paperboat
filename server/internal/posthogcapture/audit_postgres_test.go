package posthogcapture

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
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
	w := audit.NewWriter(store)
	write := func(e audit.Event) {
		t.Helper()
		if err := w.Write(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	write(audit.Event{ID: "login_" + x, ActorUserID: "acct_" + x, ActorType: audit.ActorUser, EventType: "auth.login", ResourceType: "user", ResourceID: "acct_" + x})
	write(audit.Event{ID: "grant_" + x, ActorUserID: "operator_" + x, ActorType: audit.ActorAdmin, EventType: "operator.plan_grant", ResourceType: "billing_workspace", ResourceID: "personal:owner_" + x,
		Metadata: map[string]any{"product_code": "pro", "state": "granted"}})
	write(audit.Event{ID: "team_" + x, ActorUserID: "operator_" + x, ActorType: audit.ActorAdmin, EventType: "operator.plan_revoke", ResourceType: "billing_workspace", ResourceID: "team:missing_" + x})
	write(audit.Event{ID: "logout_" + x, ActorUserID: "acct_" + x, ActorType: audit.ActorUser, EventType: "auth.logout", ResourceType: "user", ResourceID: "acct_" + x})
	write(audit.Event{ID: "denied_" + x, ActorUserID: "acct_" + x, ActorType: audit.ActorUser, EventType: "auth.login", ResourceType: "user", ResourceID: "acct_" + x, Result: audit.ResultDenied})
	if _, err := store.SQL().ExecContext(ctx, `UPDATE audit_events SET created_at = now() - interval '5 minutes' WHERE id LIKE '%\_'||$1`, x); err != nil {
		t.Fatal(err)
	}
	write(audit.Event{ID: "young_" + x, ActorUserID: "acct_" + x, ActorType: audit.ActorUser, EventType: "auth.login", ResourceType: "user", ResourceID: "acct_" + x})

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
