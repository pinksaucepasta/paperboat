package teams

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// This public-service boundary catches the old 24-hour expiry and sender items
// leaking into the recipient inbox.
func TestPostgresInvitationThirtyDayInboxPolicy(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, recipient, team := "mailowner_"+suffix, "mailrecipient_"+suffix, "mail-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tx, e := store.SQL().BeginTx(clean, nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		for _, q := range []string{
			`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`,
			`DELETE FROM paperboat.audit_events WHERE actor_user_id=$1 OR actor_user_id=$2`,
			`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1 AND $2<>''`,
			`DELETE FROM paperboat.teams WHERE team_id=$1 AND $2<>''`,
			`DELETE FROM paperboat.users WHERE id=$1 OR id=$2`,
			`ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`,
		} {
			args := []any{owner, recipient}
			if q[0:12] == "ALTER TABLE " {
				args = nil
			}
			if strings.Contains(q, "team_id=$1") {
				args = []any{team, owner}
			}
			if _, e = tx.ExecContext(clean, q, args...); e != nil {
				t.Error(e)
				return
			}
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	})

	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) SELECT x,x,x||'@example.test',true,'active' FROM unnest($1::text[]) x`, []string{owner, recipient})
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',3,now()-interval '1 hour',now()+interval '60 days')`, "bw_"+suffix, team)
	// The isolated mail database is private test data; unique IDs avoid other tests.
	s := NewService(store)
	current, err := s.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := s.Invite(ctx, owner, team, InviteRequest{OperationID: "invite", ExpectedGeneration: current.Generation, AccountID: recipient})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("thirty days", func(t *testing.T) {
		remaining := time.Until(invite.ExpiresAt)
		if remaining < 30*24*time.Hour-time.Minute || remaining > 30*24*time.Hour {
			t.Fatalf("invitation expires in %s; want 30 days", remaining)
		}
	})
	t.Run("received excludes own created", func(t *testing.T) {
		items, total, e := s.InvitationInventory(ctx, owner, "", "", "", "", "", 50, 0)
		if e != nil || total != 0 || len(items) != 0 {
			t.Fatalf("sender inbox has %d items: %v", total, e)
		}
	})
	t.Run("recipient inbox", func(t *testing.T) {
		items, total, e := s.InvitationInventory(ctx, recipient, "", "", "", "", "", 50, 0)
		if e != nil || total != 1 || len(items) != 1 || items[0].InvitationID != invite.InvitationID {
			t.Fatalf("recipient inbox count=%d: %v", total, e)
		}
	})
}
