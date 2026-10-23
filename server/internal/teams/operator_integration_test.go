package teams

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestOperatorTeamSuspensionFencesAuthorityAndResumesAtNewGeneration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	var actorBytes [16]byte
	if _, err := rand.Read(actorBytes[:]); err != nil {
		t.Fatal(err)
	}
	admin := "opr_" + hex.EncodeToString(actorBytes[:])
	owner, memberID, team := "usr_owner_"+suffix, "usr_member_"+suffix, "team-suspend-"+suffix
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@example.test','unused test login','active','admin')`, []any{admin}},
		{`INSERT INTO users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@example.test','active','user'),($2,$2,$2||'@example.test','active','user')`, []any{owner, memberID}},
		{`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, []any{team, owner}},
		{`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, []any{team, owner, memberID}},
		{`INSERT INTO team_resource_bindings(team_id,resource_kind,resource_id,owner_account,generation,active) VALUES($1,'env',$1,$2,1,true)`, []any{team, owner}},
		{`INSERT INTO billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES('team:'||$1,$1,'active',2,now()-interval '1 day',now()+interval '1 day')`, []any{team}},
	} {
		if _, err = store.Pool().Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM billing_workspaces WHERE team_id=$1`, `DELETE FROM team_resource_grants WHERE team_id=$1`, `DELETE FROM workspace_policies WHERE team_id=$1`, `DELETE FROM team_resource_bindings WHERE team_id=$1`, `DELETE FROM team_members WHERE team_id=$1`, `DELETE FROM teams WHERE team_id=$1`} {
			store.Pool().Exec(context.Background(), q, team)
		}
		store.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=ANY($1)`, []string{owner, memberID})
		store.Pool().Exec(context.Background(), `DELETE FROM operator_accounts WHERE id=$1`, admin)
	})
	setENVAccess(t, store, team, memberID, "read")
	service := NewService(store)
	before, err := service.Authorize(ctx, memberID, team, "env", team, "read")
	if err != nil {
		t.Fatal(err)
	}
	var suspendedGeneration int64
	err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var e error
		suspendedGeneration, e = SetOperatorSuspensionTx(ctx, tx, admin, team, true, int64(before.TeamGeneration))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Authorize(ctx, memberID, team, "env", team, "read"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("suspended authorization err=%v", err)
	}
	for _, attempt := range []func() error{
		func() error {
			_, e := service.Mutate(ctx, "outside_account", team, MutationRequest{OperationID: "outside_suspend", ExpectedGeneration: uint64(suspendedGeneration), Action: "rename", Name: "Changed"})
			return e
		},
		func() error {
			_, e := service.Invite(ctx, "outside_account", team, InviteRequest{OperationID: "outside_invite", ExpectedGeneration: uint64(suspendedGeneration), AccountID: memberID})
			return e
		},
	} {
		if e := attempt(); !errors.Is(e, ErrForbidden) {
			t.Fatalf("suspended team leaked to outsider: %v", e)
		}
	}
	if _, e := service.Mutate(ctx, owner, team, MutationRequest{OperationID: "suspended_rename", ExpectedGeneration: uint64(suspendedGeneration), Action: "rename", Name: "Changed"}); !errors.Is(e, ErrSuspended) {
		t.Fatalf("suspended mutation error=%v", e)
	}
	if state, e := service.Get(ctx, owner, team); e != nil || !state.Suspended {
		t.Fatalf("current member cannot read suspended state: suspended=%v err=%v", state.Suspended, e)
	}
	if _, e := service.Get(ctx, "outside_account", team); !errors.Is(e, ErrForbidden) {
		t.Fatalf("outsider read suspended team: %v", e)
	}
	if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, e := SetOperatorSuspensionTx(ctx, tx, admin, team, false, int64(before.TeamGeneration))
		return e
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation err=%v", err)
	}
	var resumedGeneration int64
	err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var e error
		resumedGeneration, e = SetOperatorSuspensionTx(ctx, tx, admin, team, false, suspendedGeneration)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumedGeneration != suspendedGeneration+1 {
		t.Fatalf("resume generation=%d want %d", resumedGeneration, suspendedGeneration+1)
	}
	after, err := service.Authorize(ctx, memberID, team, "env", team, "read")
	if err != nil {
		t.Fatal(err)
	}
	if after.TeamGeneration != uint64(resumedGeneration) || after.TeamGeneration == before.TeamGeneration {
		t.Fatalf("authority generation before=%d after=%d", before.TeamGeneration, after.TeamGeneration)
	}
}
