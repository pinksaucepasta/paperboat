package teams

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Membership changes lock only their own team (FIX-TEAMLOCK2). Every ENV vault transaction holds ROW SHARE on the
// global team write fence (db.LockTeamsFor); while one is open, creating a team, inviting, accepting, resending,
// declining and removing a member must all complete. With the global lock (teams.Lock) each of them waited for
// every account's ENV, and every account's ENV waited for them.
func TestPostgresMembershipChangesDoNotWaitForOtherAccountsENV(t *testing.T) {
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
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	owner, joiner, decliner, team := "tl_owner_"+suffix, "tl_join_"+suffix, "tl_decline_"+suffix, "tl-team-"+suffix
	people := []string{owner, joiner, decliner}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		tx, e := store.SQL().BeginTx(clean, nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		for _, item := range []struct {
			query string
			args  []any
		}{
			{`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`, nil},
			{`DELETE FROM paperboat.audit_events WHERE actor_user_id=ANY($1::text[])`, []any{people}},
			{`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id=ANY($1::text[])`, []any{people}},
			{`ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`, nil},
		} {
			if _, e = tx.ExecContext(clean, item.query, item.args...); e != nil {
				t.Error(e)
				return
			}
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	}()
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, people); err != nil {
		t.Fatal(err)
	}

	// Another account's ENV transaction stays open for the whole test.
	held := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := db.LockTeamsFor(ctx, tx, false, "tl-other-"+suffix); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	step := func(name string, f func(context.Context) error) {
		t.Helper()
		c, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		if err := f(c); err != nil {
			t.Fatalf("%s while another account's ENV transaction is open: %v", name, err)
		}
	}
	s := NewService(store)
	var state Team
	step("create", func(c context.Context) (err error) {
		state, err = s.Create(c, owner, CreateRequest{OperationID: "create", TeamID: team})
		return err
	})
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',5,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team); err != nil {
		t.Fatal(err)
	}
	invite := func(account, op string) (i Invitation) {
		t.Helper()
		step("invite "+account, func(c context.Context) (err error) {
			if state, err = s.Get(c, owner, team); err != nil {
				return err
			}
			i, err = s.Invite(c, owner, team, InviteRequest{OperationID: op, ExpectedGeneration: state.Generation, AccountID: account})
			return err
		})
		return i
	}
	joined := invite(joiner, "invite_join")
	step("resend", func(c context.Context) error {
		_, err := s.ResendInvitation(c, owner, team, joined.InvitationID, AcceptRequest{OperationID: "resend"})
		var cooldown *RetryAfterError
		if errors.As(err, &cooldown) {
			return nil // it ran to its cooldown check under the lock
		}
		return err
	})
	step("accept", func(c context.Context) (err error) {
		state, err = s.Accept(c, joiner, joined.InvitationID, AcceptRequest{OperationID: "accept"})
		return err
	})
	declined := invite(decliner, "invite_decline")
	step("decline", func(c context.Context) error {
		_, err := s.DeclineInvitation(c, decliner, declined.InvitationID, AcceptRequest{OperationID: "decline"})
		return err
	})
	step("remove", func(c context.Context) (err error) {
		if state, err = s.Get(c, owner, team); err != nil {
			return err
		}
		_, err = s.Mutate(c, owner, team, MutationRequest{OperationID: "remove", ExpectedGeneration: state.Generation, Action: "remove", AccountID: joiner})
		return err
	})
}
