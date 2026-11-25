package usermachines

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
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
			`DELETE FROM paperboat.workspace_policies WHERE team_id=ANY($1)`,
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
	machine("team_tagged", member, team, "online")  // reached by a group rule on a tag
	machine("team_unruled", member, team, "online") // no rule: only its team's owner and admins (management)
	machine("lent", outsider, nil, "online")        // personal, bound, no rule of its own: the admin's every-machine rule
	// Shortcut edges: a revoked own machine, and a machine owned by a suspended team, must get exactly what the
	// full access functions answer.
	exec(`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, m("own_revoked"))
	exec(`UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, other, owner)
	for _, id := range []string{m("team_all"), m("team_picked"), m("team_none"), m("shared"), m("team_tagged"), m("team_unruled"), m("lent")} {
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'machine',id,user_id,true,1 FROM paperboat.user_machines WHERE id=$2`, team, id)
	}
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, other, m("other_team"), owner)
	// Machine access is the team's workspace policy (migration 128).
	exec(`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'{}','sha256:test')`, team)
	exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
SELECT p.id,0,false,r.kind,r.subject,'machine',r.machine,r.action,int4range(1,65536) FROM paperboat.workspace_policies p,
 (VALUES('role','member',$2::text,'terminal'),('role','member',$3::text,'exec'),('account',$4::text,$5::text,'files')) r(kind,subject,machine,action)
WHERE p.team_id=$1`, team, m("team_all"), m("shared"), picked, m("team_picked"))
	// The other rule shapes: a group on a tag, one account on every machine, and a deny that wins over an allow.
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT id,'build',$2 FROM paperboat.workspace_policies WHERE team_id=$1`, team, m("team_tagged"))
	exec(`INSERT INTO paperboat.workspace_policy_groups(policy_id,group_name,account_id) SELECT id,'ops',$2 FROM paperboat.workspace_policies WHERE team_id=$1`, team, picked)
	exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
SELECT p.id,1,r.deny,r.kind,r.subject,r.target_kind,r.target,r.action,int4range(1,65536) FROM paperboat.workspace_policies p,
 (VALUES(false,'group','ops','tag','build','managed_ssh'),(false,'account',$4::text,'all_machines','','tunnel_manage'),(true,'account',$2::text,'machine',$3::text,'terminal')) r(deny,kind,subject,target_kind,target,action)
WHERE p.team_id=$1`, team, member, m("team_all"), admin)

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
	// The access description as the functions alone give it (all workspaces, or Personal when personal is set).
	describedAccess := func(actor, machine string, personal bool) (bool, []string) {
		var manage bool
		var raw []byte
		if err := store.SQL().QueryRowContext(ctx, `SELECT CASE WHEN $3 THEN m.user_id=$1 AND m.owner_team_id IS NULL ELSE paperboat.machine_management_allowed($1,m.id) END,
 coalesce(json_agg(c ORDER BY n) FILTER (WHERE CASE WHEN $3 THEN m.user_id=$1 AND m.owner_team_id IS NULL AND paperboat.machine_capability_allowed($1,m.id,c) ELSE paperboat.machine_capability_allowed($1,m.id,c) END),'[]'::json)
 FROM paperboat.user_machines m CROSS JOIN unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) WITH ORDINALITY AS caps(c,n)
 WHERE m.id=$2 GROUP BY m.id,m.user_id,m.owner_team_id`, actor, machine, personal).Scan(&manage, &raw); err != nil {
			t.Fatal(err)
		}
		var permissions []string
		if err := json.Unmarshal(raw, &permissions); err != nil {
			t.Fatal(err)
		}
		return manage, permissions
	}
	// Revoked and disconnected machines are left out of the default list and its total, and counted apart.
	revoked := map[string]bool{}
	revokedRows, err := store.SQL().QueryContext(ctx, `SELECT id FROM paperboat.user_machines WHERE id LIKE 'ic_machine_%_'||$1 AND (revoked_at IS NOT NULL OR state IN ('revoked','disconnected'))`, s)
	if err != nil {
		t.Fatal(err)
	}
	for revokedRows.Next() {
		var id string
		if err := revokedRows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		revoked[id] = true
	}
	revokedRows.Close()
	if len(revoked) == 0 {
		t.Fatal("fixture has no revoked machine")
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
			allowed := expected(actor, scoped, teamID)
			var want []string
			for _, id := range allowed {
				if !revoked[id] {
					want = append(want, id)
				}
			}
			items, total, _, err := service.ListInventory(listCtx, actor, InventoryOptions{Limit: 200})
			if err != nil {
				if scope != nil && scope.TeamID != "" && len(allowed) == 0 {
					continue // not a member: the workspace itself is refused
				}
				t.Fatalf("%s in %v: %v", actor, scope, err)
			}
			var got []string
			for _, item := range items {
				if item.ID[len(item.ID)-len(s):] == s {
					got = append(got, item.ID)
					if scope == nil || scope.TeamID == "" {
						manage, permissions := describedAccess(actor, item.ID, scope != nil)
						if item.CanManage != manage || !slices.Equal(item.Permissions, permissions) {
							t.Fatalf("%s in %v: %s described manage=%v %v, access functions say %v %v", actor, scope, item.ID, item.CanManage, item.Permissions, manage, permissions)
						}
					}
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) || total != len(items) {
				t.Fatalf("%s in %v: listed %v (total %d), access rules allow %v (without revoked)", actor, scope, got, total, want)
			}
			// The summary counts what the list authorizes, the whole workspace (fixture machines are all linux).
			summary, err := service.Summary(listCtx, actor)
			if err != nil {
				t.Fatalf("%s in %v: summary: %v", actor, scope, err)
			}
			states := 0
			for state, n := range summary.ByState {
				if state != "revoked" {
					states += n
				}
			}
			if summary.Total != len(want) || summary.ByState["revoked"] != len(allowed)-len(want) || summary.ByPlatform["linux"] != len(want) || states != len(want) {
				t.Fatalf("%s in %v: summary %+v; the list allows %d machines in use and %d revoked", actor, scope, summary, len(want), len(allowed)-len(want))
			}
			nonEmpty += min(len(want), 1)
		}
	}
	if nonEmpty < 8 {
		t.Fatalf("fixture too weak: only %d actor/workspace pairs see any machine", nonEmpty)
	}

	// Differential: the list, capabilities and managed SSH equal the SQL that called the access functions, at every
	// stage of the team. listInventory is called directly: workspace validation would refuse a suspended team first.
	exec(`INSERT INTO paperboat.machine_ssh_targets(user_machine_id,machine_generation,os_user,target_port,created_at,updated_at)
 SELECT id,installation_generation,'pbt',22,now(),now() FROM paperboat.user_machines WHERE id LIKE 'ic_machine_%_'||$1`, s)
	exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
SELECT id,2,false,'role','member','all_machines','','private_access',int4range(22,23) FROM paperboat.workspace_policies WHERE team_id=$1`, team)
	if inventoryDifferential(t, store, service, users, team, other, s) < 8 {
		t.Fatal("differential fixture too weak: hardly any team capabilities")
	}
	for _, stage := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE paperboat.workspace_policies SET allow_private_access=true WHERE team_id=$1`, []any{team}},
		{`UPDATE paperboat.workspace_policy_rules SET subject_kind='all',subject_id='' WHERE rule=2 AND policy_id IN (SELECT id FROM paperboat.workspace_policies WHERE team_id=$1)`, []any{team}},
		{`UPDATE paperboat.team_resource_bindings SET active=false WHERE resource_id=$1`, []any{m("shared")}},
		{`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, []any{m("team_tagged")}},
		{`UPDATE paperboat.user_machines SET deleted_at=now() WHERE id=$1`, []any{m("team_picked")}},
		{`UPDATE paperboat.teams SET sso_required=true,sso_session_seconds=60 WHERE team_id=$1`, []any{team}},
		{`UPDATE paperboat.teams SET sso_required=false WHERE team_id=$1`, []any{team}},
		{`UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=owner_account WHERE team_id=$1`, []any{team}},
		{`UPDATE paperboat.teams SET suspended=false,suspended_at=NULL,suspended_by=NULL WHERE team_id=$1`, []any{team}},
		{`UPDATE paperboat.team_members SET active=false WHERE account_id=$1`, []any{picked}},
		{`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, []any{team}},
	} {
		exec(stage.sql, stage.args...)
		t.Log(stage.sql)
		inventoryDifferential(t, store, service, users, team, other, s)
	}
}

// inventoryDifferential compares every actor's list in every workspace with the SQL before the access functions were
// taken apart (2026-11-25), kept here as the oracle. It returns how many team capability arrays were not empty.
func inventoryDifferential(t *testing.T, store *db.DB, service *Service, users []string, team, other, suffix string) int {
	t.Helper()
	ctx, capable := t.Context(), 0
	const newRule = `(team_machine_bound_for($1,$3,m.id) AND EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE NOT isempty(workspace_policy_rule_ports($1,$3,m.id,cap))))`
	const oldRule = `EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE team_machine_capability_allowed($1,$3,m.id,cap))`
	for _, actor := range users {
		for _, scope := range []*workspace.Scope{nil, {ID: "personal"}, {ID: team, TeamID: team}, {ID: other, TeamID: other}} {
			listCtx, scoped, teamID := ctx, scope != nil, any(nil)
			if scope != nil {
				listCtx = workspace.WithContext(ctx, *scope)
				if scope.TeamID != "" {
					teamID = scope.TeamID
				}
			}
			for _, opts := range []InventoryOptions{{Limit: 200}, {Limit: 1}, {Limit: 2, Offset: 1}, {Limit: 1, Offset: 100}, {Limit: 5, After: &InventoryPosition{Key: "shared", ID: ""}}, {Limit: 9, Q: "team"}, {Limit: 9, Owner: "shared"}} {
				with, predicate, args := inventoryFilter(listCtx, actor, opts)
				if strings.Count(predicate, newRule) != 1 {
					t.Fatal("the team rule changed: update the differential")
				}
				var want []string
				wantTotal := 0
				rows, err := store.SQL().QueryContext(ctx, with+`SELECT m.id,lower(m.alias)`+strings.Replace(predicate, newRule, oldRule, 1)+` ORDER BY lower(m.alias),m.id`, args...)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var id, key string
					if err := rows.Scan(&id, &key); err != nil {
						t.Fatal(err)
					}
					wantTotal++
					if opts.After == nil || key > opts.After.Key || key == opts.After.Key && id > opts.After.ID {
						want = append(want, id)
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				want = want[min(opts.Offset, len(want)):]
				want = want[:min(opts.Limit+1, len(want))]
				items, total, err := service.listInventory(listCtx, actor, opts)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, item := range items {
					got = append(got, item.ID)
				}
				if !slices.Equal(got, want) || total != wantTotal {
					t.Fatalf("%s in %v %+v: listed %v (total %d), old SQL %v (%d)", actor, scope, opts, got, total, want, wantTotal)
				}
			}
			// Every fixture machine is described, listed or not: the description authorizes on its own.
			var all []string
			if err := store.SQL().QueryRowContext(ctx, `SELECT array_agg(id) FROM paperboat.user_machines WHERE id LIKE 'ic_machine_%_'||$1`, suffix).Scan(pgtype.NewMap().SQLScanner(&all)); err != nil {
				t.Fatal(err)
			}
			for _, id := range all {
				var manage, ssh bool
				var raw []byte
				if err := store.SQL().QueryRowContext(ctx, `SELECT
 CASE WHEN NOT $3::boolean THEN machine_management_allowed($1,m.id) WHEN $4::text IS NULL THEN m.user_id=$1 AND m.owner_team_id IS NULL
      ELSE coalesce(m.owner_team_id=$4 AND machine_management_allowed($1,m.id),false) END,
 coalesce((SELECT json_agg(capability) FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) capability WHERE
   CASE WHEN NOT $3::boolean THEN (m.owner_team_id IS NULL AND m.user_id=$1 AND m.revoked_at IS NULL AND m.deleted_at IS NULL) OR machine_capability_allowed($1,m.id,capability)
        WHEN $4::text IS NULL THEN m.user_id=$1 AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL
        ELSE team_machine_capability_allowed($1,$4,m.id,capability) END),'[]'::json),
 m.state NOT IN ('revoked','disconnected','deleted') AND ((m.owner_team_id IS NULL AND m.user_id=$1 AND m.revoked_at IS NULL AND m.deleted_at IS NULL)
  OR machine_capability_allowed($1,m.id,'managed_ssh') OR machine_management_allowed($1,m.id))
 FROM user_machines m WHERE m.id=$2`, actor, id, scoped, teamID).Scan(&manage, &raw, &ssh); err != nil {
					t.Fatal(err)
				}
				var permissions []string
				if err := json.Unmarshal(raw, &permissions); err != nil {
					t.Fatal(err)
				}
				item := &UserMachine{ID: id}
				if err := service.describeMachineAccess(listCtx, actor, item); err != nil {
					t.Fatal(err)
				}
				if err := service.describeManagedSSH(listCtx, actor, item); err != nil {
					t.Fatal(err)
				}
				if item.CanManage != manage || !slices.Equal(item.Permissions, permissions) || (item.SSH != nil) != ssh {
					t.Fatalf("%s in %v: %s is %v %v ssh=%v, old SQL %v %v %v", actor, scope, id, item.CanManage, item.Permissions, item.SSH != nil, manage, permissions, ssh)
				}
				if teamID != nil && len(permissions) > 0 {
					capable++
				}
			}
		}
	}
	return capable
}
