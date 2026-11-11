package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A terminal session of a team workspace says which team owns it: the console offers recording by hand to its owner
// only then (it never did, because the catalog read the column and dropped it).
func TestTerminalSessionsCarryTheirOwnerTeamPostgres(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	owner, team := "tot_owner_"+suffix, "tot-team-"+suffix
	machine, teamSession, personalSession := "tot_machine_"+suffix, "tot_team_terminal_"+suffix, "tot_personal_terminal_"+suffix
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.user_machine_terminal_sessions WHERE id IN ($1,$2)`, teamSession, personalSession)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,'owner@owner.invalid',true,'active')`, owner)
	insert(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	insert(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
	insert(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities,owner_team_id)
VALUES($1,$2,$3,'owner-team-host','linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'],$4)`, machine, owner, "env_"+suffix, team)
	insert(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,owner_team_id,name,launch_cwd,desired_state,runtime_state)
VALUES($1,$2,$3,$4,'in-team','/workspace','open','running'),($5,$2,$3,NULL,'personal','/workspace','open','running')`, teamSession, machine, owner, team, personalSession)

	catalog, err := New(store, nil, Policy{}, nil).TerminalSessionCatalog(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, s := range catalog {
		seen[s.ID] = s.OwnerTeamID
	}
	if owned, ok := seen[teamSession]; !ok || owned != team {
		t.Fatalf("team session owner_team_id = %q (listed %v), want %q", owned, ok, team)
	}
	if owned, ok := seen[personalSession]; ok && owned != "" {
		t.Fatalf("a personal session carries team %q", owned)
	}
}
