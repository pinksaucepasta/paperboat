package usermachines

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

// The inventory's candidate clause (own machines, machines owned by or bound to the account's active teams) only
// narrows the rows the access functions see. For every actor and workspace the list must equal the machines the
// access functions alone allow, on a fixture that exercises every way a machine becomes visible or hidden.
func TestMachineInventoryCandidatesMatchTheAccessRulesOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	s := fmt.Sprint(time.Now().UnixNano())
	owner, admin, member, picked, outsider := "ic_owner_"+s, "ic_admin_"+s, "ic_member_"+s, "ic_picked_"+s, "ic_outsider_"+s
	team, other := "ic-team-"+s, "ic-other-"+s
	users := []string{owner, admin, member, picked, outsider}
	m := func(n string) string { return "ic_machine_" + n + "_" + s }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM paperboat.team_machine_grants WHERE team_id=ANY($1)`,
			`DELETE FROM paperboat.team_resource_bindings WHERE team_id=ANY($1)`,
			`DELETE FROM paperboat.billing_workspaces WHERE team_id=ANY($1)`,
			`DELETE FROM paperboat.user_machines WHERE owner_team_id=ANY($1)`,
			`DELETE FROM paperboat.team_members WHERE team_id=ANY($1)`,
			`DELETE FROM paperboat.teams WHERE team_id=ANY($1)`,
		} {
			_, _ = store.SQL().ExecContext(ctx, q, []string{team, other})
		}
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE user_id=ANY($1)`, users)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=ANY($1)`, users)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@example.test','active' FROM unnest($1::text[]) x`, users)
	for _, t := range []string{team, other} {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, t, owner)
		exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team',5,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+t, t)
	}
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES
($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true),($1,$5,1,'member',true),($6,$2,1,'member',true),($6,$7,1,'member',false)`,
		team, owner, admin, member, picked, other, outsider)
	machine := func(name, user string, ownerTeam any, state string) {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id,
configured_capabilities,observed_capabilities) VALUES($1,$2,$1,$3,'linux','amd64','/w',$4,'occupied',true,$5,ARRAY['terminal_host','file_receive','ssh_host'],ARRAY['terminal_host'])`,
			m(name), user, strings.ReplaceAll(name, "_", "-"), state, ownerTeam)
	}
	machine("own", member, nil, "online")
	machine("own_revoked", member, nil, "revoked")
	machine("outsider", outsider, nil, "online")
	machine("shared", outsider, nil, "online") // personal machine of a non-member bound to the team
	machine("team_all", admin, team, "online")
	machine("team_picked", owner, team, "offline")
	machine("team_none", member, team, "online")
	machine("other_team", owner, other, "online")
	for _, id := range []string{m("team_all"), m("team_picked"), m("team_none"), m("shared")} {
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'machine',id,user_id,true,1 FROM paperboat.user_machines WHERE id=$2`, team, id)
	}
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, other, m("other_team"), owner)
	exec(`INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,capabilities) VALUES($1,$2,'all_members',ARRAY['terminal']),($1,$3,'all_members',ARRAY['exec'])`, team, m("team_all"), m("shared"))
	exec(`INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,account_id,capabilities) VALUES($1,$2,'selected_member',$3,ARRAY['files'])`, team, m("team_picked"), picked)

	// The access rule alone, as the inventory evaluated it before the candidate clause.
	expected := func(actor string, scoped bool, teamID any) []string {
		rows, err := store.SQL().QueryContext(ctx, `SELECT m.id FROM paperboat.user_machines m WHERE m.id LIKE 'ic_machine_%_'||$4 AND m.deleted_at IS NULL AND (
 (NOT $2::boolean AND (paperboat.machine_management_allowed($1,m.id) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE paperboat.machine_capability_allowed($1,m.id,cap))))
 OR ($2::boolean AND $3::text IS NULL AND m.user_id=$1 AND m.owner_team_id IS NULL)
 OR ($2::boolean AND $3::text IS NOT NULL AND EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members member USING(team_id) WHERE t.team_id=$3 AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=$1 AND member.active)
 AND ((m.owner_team_id=$3 AND paperboat.machine_management_allowed($1,m.id)) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE paperboat.team_machine_capability_allowed($1,$3,m.id,cap)))))
 ORDER BY m.id`, actor, scoped, teamID, s)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	service := New(store, nil, Policy{}, nil)
	nonEmpty := 0
	for _, actor := range users {
		for _, scope := range []*workspace.Scope{nil, {ID: "personal"}, {ID: team, TeamID: team}, {ID: other, TeamID: other}} {
			listCtx, scoped, teamID := ctx, scope != nil, any(nil)
			if scope != nil {
				listCtx = workspace.WithContext(ctx, *scope)
				if scope.TeamID != "" {
					teamID = scope.TeamID
				}
			}
			want := expected(actor, scoped, teamID)
			items, total, err := service.ListInventory(listCtx, actor, InventoryOptions{Limit: 200})
			if err != nil {
				if scope != nil && scope.TeamID != "" && len(want) == 0 {
					continue // not a member: the workspace itself is refused
				}
				t.Fatalf("%s in %v: %v", actor, scope, err)
			}
			var got []string
			for _, item := range items {
				if item.ID[len(item.ID)-len(s):] == s {
					got = append(got, item.ID)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) || total != len(items) {
				t.Fatalf("%s in %v: listed %v (total %d), access rules allow %v", actor, scope, got, total, want)
			}
			nonEmpty += min(len(want), 1)
		}
	}
	if nonEmpty < 8 {
		t.Fatalf("fixture too weak: only %d actor/workspace pairs see any machine", nonEmpty)
	}
}
