package inboxdispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestEligibleMachineHonoursAuthorityAvailabilityAndCapabilityOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run inbox dispatch integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	prefix := fmt.Sprintf("inboxdisp_%d_", time.Now().UnixNano())
	owner, stranger, admin, member, team := prefix+"owner", prefix+"stranger", prefix+"admin", prefix+"member", prefix+"team"
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
			{`DELETE FROM paperboat.user_machines WHERE user_id IN($1,$2,$3,$4)`, []any{owner, stranger, admin, member}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.users WHERE id IN($1,$2,$3,$4)`, []any{owner, stranger, admin, member}},
		} {
			if _, err := store.SQL().ExecContext(context.Background(), command.query, command.args...); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
	})
	for _, actor := range []string{owner, stranger, admin, member} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, actor, actor+"@example.test")
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, team, owner, admin, member)
	personal, shared, nocap, offline := prefix+"personal", prefix+"shared", prefix+"nocap", prefix+"offline"
	insert := func(id, state string, online bool, owner_team any, capabilities string) {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state,online,owner_team_id,configured_capabilities,observed_capabilities)
 VALUES($1,$2,$1||'_env','inbox-'||substr(md5($1),1,10),'','linux','amd64','/workspace',$3,'occupied',$4,$5,$6::text[],$6::text[])`, id, owner, state, online, owner_team, capabilities)
	}
	insert(personal, "online", true, nil, "{file_receive,preview_launch}")
	insert(shared, "online", true, team, "{file_receive}")
	insert(nocap, "online", true, nil, "{preview_launch}")
	insert(offline, "offline", false, nil, "{file_receive}")

	dispatcher := &Dispatcher{db: store}
	ask := func(actor, machine string, teamID sql.NullString) (route, error) {
		return dispatcher.eligibleMachine(ctx, Request{ActorID: actor, MachineID: machine, TeamID: teamID})
	}
	teamSelected := sql.NullString{String: team, Valid: true}
	if found, err := ask(owner, personal, sql.NullString{}); err != nil || found.OwnerID != owner || found.EnvironmentID != personal+"_env" {
		t.Fatalf("owner of a personal machine: %+v %v", found, err)
	}
	for name, tc := range map[string]struct {
		actor, machine string
		team           sql.NullString
	}{
		"stranger":                {stranger, personal, sql.NullString{}},
		"team member, not admin":  {member, shared, teamSelected},
		"machine without receive": {owner, nocap, sql.NullString{}},
		"offline machine":         {owner, offline, sql.NullString{}},
		"missing machine":         {owner, prefix + "missing", sql.NullString{}},
		"personal in team scope":  {owner, personal, teamSelected},
	} {
		if _, err := ask(tc.actor, tc.machine, tc.team); !errors.Is(err, ErrMachineOffline) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, manager := range []string{owner, admin} {
		if _, err := ask(manager, shared, teamSelected); err != nil {
			t.Errorf("team manager %s: %v", manager, err)
		}
	}
}
