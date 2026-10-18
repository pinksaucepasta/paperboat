package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCanManageInboxIsOwnerOrTeamAdminOnlyOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	prefix := fmt.Sprintf("inbox_%d_", time.Now().UnixNano())
	owner, other, admin, member, team := prefix+"owner", prefix+"other", prefix+"admin", prefix+"member", prefix+"team"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, command := range []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.user_machines WHERE user_id IN($1,$2,$3,$4)`, []any{owner, other, admin, member}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id IN($1,$2,$3,$4)`, []any{owner, other, admin, member}},
		} {
			if _, err := store.SQL().ExecContext(context.Background(), command.query, command.args...); err != nil {
				t.Errorf("inbox fixture cleanup: %v", err)
			}
		}
	})
	for _, actor := range []string{owner, other, admin, member} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, team, owner, admin, member)
	personal, shared := prefix+"personal", prefix+"shared"
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state,owner_team_id) VALUES
 ($1,$3,$1||'_env','inbox-personal','','linux','amd64','/workspace','online','occupied',NULL),
 ($2,$3,$2||'_env','inbox-shared','','linux','amd64','/workspace','online','occupied',$4)`, personal, shared, owner, team)

	service := New(store, nil, Policy{}, nil)
	for _, allowed := range []struct{ actor, machine string }{{owner, personal}, {owner, shared}, {admin, shared}} {
		if err := service.CanManageInbox(ctx, allowed.actor, allowed.machine); err != nil {
			t.Errorf("%s on %s: %v", allowed.actor, allowed.machine, err)
		}
	}
	for name, denied := range map[string]struct{ actor, machine string }{
		"stranger on a personal machine": {other, personal},
		"team member who is not admin":   {member, shared},
		"admin on a personal machine":    {admin, personal},
		"missing machine":                {owner, prefix + "missing"},
	} {
		if err := service.CanManageInbox(ctx, denied.actor, denied.machine); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	exec(`UPDATE paperboat.user_machines SET deleted_at=now() WHERE id=$1`, personal)
	if err := service.CanManageInbox(ctx, owner, personal); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted machine: %v", err)
	}
}
