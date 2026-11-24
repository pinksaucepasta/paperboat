package usermachines

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// The list query before the role was computed once per row and the count came from the page: the oracle for
// TestTerminalInventoryMatchesOldQueryPostgres. Do not edit it together with terminalInventoryQuery.
const oldTerminalInventoryQuery = `WITH browser_candidates AS (
 SELECT p.terminal_session_id id FROM terminal_browser_access_policies p
 JOIN user_machine_terminal_sessions target ON target.id=p.terminal_session_id
 JOIN users actor ON actor.id=$1 AND actor.status='active'
 WHERE target.owner_account<>$1 AND target.deleted_at IS NULL AND target.desired_state='open'
 AND target.runtime_state NOT IN ('closed','exited','deleted')
 AND (NOT $3::boolean OR ($4::text IS NOT NULL AND p.audience='team' AND p.team_id=$4))
 AND ((actor.email_verified AND p.audience='specific_emails' AND (lower(actor.primary_email)=ANY(p.allowed_emails) OR ('*@'||split_part(lower(actor.primary_email),'@',2))=ANY(p.allowed_emails)))
   OR (p.audience='team' AND team_paid_entitled(p.team_id) AND EXISTS(
    SELECT 1 FROM teams team
    JOIN team_members member ON member.team_id=team.team_id AND member.account_id=$1 AND member.active
    JOIN team_members owner_member ON owner_member.team_id=team.team_id AND owner_member.account_id=target.owner_account AND owner_member.active
    WHERE team.team_id=p.team_id AND team.deleted_at IS NULL AND NOT team.suspended)))

), sessions AS (
 SELECT s.id,s.name,s.launch_cwd,s.desired_state,s.runtime_state,s.last_activity_at,s.closed_at,s.closed_reason,s.is_default,s.created_at,s.updated_at,s.owner_account,s.owner_team_id,'machine'::text kind,m.id target_id,m.alias target_name,m.id machine_id,m.environment_id,m.installation_generation,m.state,m.online,m.configured_capabilities @> ARRAY['terminal_host'] AND m.observed_capabilities @> ARRAY['terminal_host'] terminal_ready FROM user_machine_terminal_sessions s JOIN user_machines m ON m.id=s.user_machine_id WHERE s.deleted_at IS NULL AND m.deleted_at IS NULL
 AND (s.owner_account=$1 OR s.id IN (SELECT id FROM browser_candidates) OR s.id IN (SELECT b.resource_id FROM team_resource_bindings b
  JOIN team_members tm ON tm.team_id=b.team_id AND tm.account_id=$1 AND tm.active WHERE b.resource_kind='terminal_session' AND b.active)))
	 SELECT x.id,x.name,x.launch_cwd,x.desired_state,x.runtime_state,x.last_activity_at,x.closed_at,x.closed_reason,x.is_default,x.owner_account,x.kind,x.target_id,x.target_name,x.machine_id,x.environment_id,x.installation_generation,x.state,x.online,x.terminal_ready,coalesce(b.team_id,''),coalesce(t.generation,0),coalesce(terminal_session_role($1,x.id),''),x.owner_account=$1,EXISTS(SELECT 1 FROM team_terminal_session_grants ag WHERE ag.team_id=b.team_id AND ag.terminal_session_id=x.id AND ag.audience='all_members' AND ag.active),bc.id IS NOT NULL,x.created_at,x.updated_at,coalesce(x.owner_team_id,'')
	 FROM sessions x LEFT JOIN team_resource_bindings b ON b.resource_kind='terminal_session' AND b.resource_id=x.id AND b.active LEFT JOIN teams t ON t.team_id=b.team_id AND t.deleted_at IS NULL
	 LEFT JOIN browser_candidates bc ON bc.id=x.id
	WHERE ($2::text[] IS NULL OR x.id=ANY($2))
	  AND (NOT $3::boolean
	    OR ($4::text IS NULL AND x.owner_account=$1 AND x.owner_team_id IS NULL)
	    OR ($4::text IS NOT NULL AND (
	      (x.owner_account=$1 AND x.owner_team_id=$4)
	      OR (b.team_id=$4 AND (x.owner_account=$1 OR terminal_session_role($1,x.id) IN ('viewer','interactive')))
	      OR bc.id IS NOT NULL)))
	  AND (x.owner_account=$1 OR terminal_session_role($1,x.id) IN ('viewer','interactive') OR bc.id IS NOT NULL)
 AND ($5='' OR position(lower($5) in lower(x.name||' '||x.target_name||' '||x.id))>0)
 AND ($6='' OR CASE WHEN x.desired_state<>'open' OR x.runtime_state IN ('closed','exited','deleted') THEN 'closed' WHEN x.runtime_state='running' THEN 'running' ELSE 'unavailable' END=$6)
 AND ($7='' OR ($7='mine' AND x.owner_account=$1) OR ($7='shared' AND x.owner_account<>$1) OR x.owner_account=$7)
 AND ($8::boolean IS NULL OR (x.desired_state<>'open' OR x.runtime_state IN ('closed','exited','deleted'))=$8)
 AND ($9='' OR x.machine_id=$9)`

// Authorization of the terminal list: the query returns exactly the rows, order and count of the old one, for every
// viewer, filter, workspace and page.
func TestTerminalInventoryMatchesOldQueryPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	s := f.suffix
	id := func(name string) string { return "ti" + name + s }
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.store.Pool().Exec(t.Context(), query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM paperboat.team_resource_bindings WHERE resource_id LIKE $1`,
			`DELETE FROM paperboat.user_machine_terminal_sessions WHERE id LIKE $1`,
			`DELETE FROM paperboat.user_machines WHERE id LIKE $1`,
			`DELETE FROM paperboat.billing_workspaces WHERE id LIKE $1`,
			`DELETE FROM paperboat.team_members WHERE team_id LIKE $1 OR account_id LIKE $1`,
			`DELETE FROM paperboat.teams WHERE team_id LIKE $1`,
			`DELETE FROM paperboat.users WHERE id LIKE $1`,
		} {
			if _, err := f.store.Pool().Exec(ctx, q, "ti%"+s); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	now := time.Now()
	owner, other := id("owner"), id("other")
	people := []string{owner, other, id("selected"), id("all"), id("revoked"), id("removed"), id("stranger"), id("mail")}
	for _, u := range people {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,email_verified) VALUES($1,$1,$1||'@invalid.test','active',true)`, u)
	}
	// Teams are inserted in their final state, so no revocation trigger tidies the bindings and the query alone decides.
	teams := []string{"paid", "second", "free", "expired", "suspended", "deleted"}
	for _, name := range teams {
		team := id(name)
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,suspended,suspended_at,suspended_by,deleted_at) VALUES($1,$2,1,$3,CASE WHEN $3 THEN now() END,CASE WHEN $3 THEN $2 END,CASE WHEN $4 THEN now() END)`, team, owner, name == "suspended", name == "deleted")
		for _, u := range people[:6] {
			exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',$3)`, team, u, u != id("removed"))
		}
		end := now.Add(time.Hour)
		if name == "expired" {
			end = now.Add(-time.Minute)
		}
		if name != "free" {
			exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$1,'active','team-pro',8,$2,$3)`, team, now.Add(-time.Hour), end)
		}
	}
	for _, m := range []struct {
		name, owner, state string
		deleted            bool
	}{{"mone", owner, "online", false}, {"mtwo", other, "online", false}, {"mgone", owner, "online", true}, {"mrevoked", owner, "revoked", false}} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities,deleted_at) VALUES($1,$2,$1,'linux','amd64','/workspace',$3,'occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'],CASE WHEN $4 THEN now() END)`, id(m.name), m.owner, m.state, m.deleted)
	}
	policy := func(session, audience, team string, emails ...string) {
		t.Helper()
		exec(`INSERT INTO paperboat.terminal_browser_access_policies(terminal_session_id,audience,team_id,allowed_emails) VALUES($1,$2,nullif($3,''),coalesce($4::text[],'{}'))
ON CONFLICT(terminal_session_id) DO UPDATE SET audience=excluded.audience,team_id=excluded.team_id,allowed_emails=excluded.allowed_emails`, session, audience, team, emails)
	}
	runtimeStates := []string{"running", "exited", "unknown"}
	n := 0
	session := func(name, machine, owner, ownerTeam string) string {
		t.Helper()
		sid := id("s" + name)
		exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,idempotency_key,launch_cwd,runtime_state,owner_team_id) VALUES($1,$2,$3,$4,$1,'/workspace',$5,nullif($6,''))`, sid, id(machine), owner, name, runtimeStates[n%3], ownerTeam)
		n++
		return sid
	}
	bind := func(sid, team, owner string, active bool) {
		t.Helper()
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,terminal_process_generation) VALUES($1,'terminal_session',$2,$3,$4,1)`, team, sid, owner, active)
	}
	grant := func(sid, team, account, role string, active bool) {
		t.Helper()
		audience := "selected_member"
		if account == "" {
			audience = "all_members"
		}
		exec(`INSERT INTO paperboat.team_terminal_session_grants(team_id,terminal_session_id,audience,account_id,role,active) VALUES($1,$2,$3,nullif($4,''),$5,$6)`, team, sid, audience, account, role, active)
	}
	for _, machine := range []string{"mone", "mtwo", "mgone", "mrevoked"} {
		machineOwner := owner
		if machine == "mtwo" {
			machineOwner = other
		}
		session(machine+"-private", machine, machineOwner, "")
		deleted := session(machine+"-deleted", machine, machineOwner, "")
		bind(deleted, id("paid"), machineOwner, true)
		grant(deleted, id("paid"), "", "viewer", true)
		exec(`UPDATE paperboat.user_machine_terminal_sessions SET deleted_at=now() WHERE id=$1`, deleted)
		for _, team := range teams {
			for _, shape := range []string{"none", "unbound", "all-viewer", "all-interactive", "all-revoked", "selected", "selected-over-all"} {
				sid := session(machine+"-"+team+"-"+shape, machine, machineOwner, id(team))
				bind(sid, id(team), machineOwner, shape != "unbound")
				switch shape {
				case "unbound", "all-viewer":
					grant(sid, id(team), "", "viewer", true)
				case "all-interactive":
					grant(sid, id(team), "", "interactive", true)
				case "all-revoked":
					grant(sid, id(team), "", "viewer", false)
				case "selected", "selected-over-all":
					grant(sid, id(team), id("selected"), "interactive", true)
					grant(sid, id(team), id("revoked"), "viewer", false)
					grant(sid, id(team), id("removed"), "viewer", true)
					if shape == "selected-over-all" {
						grant(sid, id(team), "", "viewer", true)
					}
				}
			}
			policy(session(machine+"-"+team+"-browser", machine, machineOwner, id(team)), "team", id(team))
		}
		policy(session(machine+"-mail", machine, machineOwner, ""), "specific_emails", "", id("mail")+"@invalid.test", id("selected")+"@invalid.test")
		closed := session(machine+"-closed", machine, machineOwner, "")
		exec(`UPDATE paperboat.user_machine_terminal_sessions SET desired_state='closed',runtime_state='closed',closed_reason='user_closed',closed_at=now() WHERE id=$1`, closed)
	}
	// Bound to two teams: one row per binding.
	twice := session("twice", "mone", owner, "")
	for _, team := range []string{"paid", "second"} {
		bind(twice, id(team), owner, true)
	}
	grant(twice, id("paid"), "", "viewer", true)
	grant(twice, id("second"), id("selected"), "interactive", true)

	collect := func(query string, args ...any) [][]any {
		t.Helper()
		rows, err := f.store.Pool().Query(t.Context(), query, args...)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]any, error) { return row.Values() })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	yes, no := true, false
	filters := []InventoryOptions{{}, {Q: "SELECTED"}, {State: "closed"}, {State: "running"}, {State: "unavailable"}, {Owner: "mine"}, {Owner: "shared"}, {Owner: owner}, {Closed: &yes}, {Closed: &no}, {MachineID: id("mtwo")}, {Q: "matches-nothing"}}
	scopes := []string{"", "personal"}
	for _, team := range teams {
		scopes = append(scopes, id(team))
	}
	seen := map[string]int{}
	for _, viewer := range people {
		for _, scope := range scopes {
			for _, filter := range filters {
				for _, only := range [][]string{nil, {twice, id("smtwo-paid-all-viewer"), id("sabsent")}} {
					if only != nil && !reflect.DeepEqual(filter, InventoryOptions{}) {
						continue
					}
					var team, closed any
					if scope != "" && scope != "personal" {
						team = scope
					}
					if filter.Closed != nil {
						closed = *filter.Closed
					}
					args := []any{viewer, only, scope != "", team, filter.Q, filter.State, filter.Owner, closed, filter.MachineID}
					total := int(collect("SELECT count(*) FROM ("+oldTerminalInventoryQuery+") inventory", args...)[0][0].(int64))
					if got := int(collect("SELECT count(*) FROM ("+terminalInventoryQuery+") inventory", args...)[0][0].(int64)); got != total {
						t.Fatalf("%v: count %d, old %d", args, got, total)
					}
					order := " ORDER BY lower(x.name),x.id"
					if (filter.Closed != nil && *filter.Closed) || filter.State == "closed" {
						order = " ORDER BY coalesce(x.closed_at,x.updated_at) DESC,x.id"
					}
					// b.team_id settles the order of a session bound to two teams, which the service leaves open.
					order += ",b.team_id LIMIT $10 OFFSET $11"
					pages := [][2]int{{512, 0}, {3, total + 5}}
					if total > 0 {
						pages = append(pages, [][2]int{{1, 0}, {3, 2}, {3, total - 1}, {3, total}}...)
					}
					for _, page := range pages {
						pageArgs := append(append([]any{}, args...), page[0], page[1])
						want, got := collect(oldTerminalInventoryQuery+order, pageArgs...), collect(terminalInventoryQuery+order, pageArgs...)
						if len(got) != len(want) {
							t.Fatalf("%v: %d rows, old %d", pageArgs, len(got), len(want))
						}
						for i, row := range got {
							last := len(row) - 1
							if row[last] != int64(total) || !reflect.DeepEqual(row[:last], want[i]) {
								t.Fatalf("%v row %d:\n new %v\n old %v count %d", pageArgs, i, row, want[i], total)
							}
							if page[0] == 512 {
								seen[strings.TrimSuffix(viewer, s)+":"+row[21].(string)]++
							}
						}
						if only == nil && scope == "" && page[0] == 3 {
							opts := filter
							opts.Limit, opts.Offset, opts.WithoutLiveMetadata = page[0], page[1], true
							if _, got, err := f.service.TerminalSessionInventory(t.Context(), viewer, opts); err != nil || got != total {
								t.Fatalf("%v: service total %d, old %d, err %v", pageArgs, got, total, err)
							}
						}
					}
				}
			}
		}
	}
	// The fixtures reach every outcome: a comparison of empty lists would prove nothing.
	for _, want := range []string{"tiowner:owner", "tiall:viewer", "tiall:interactive", "tiselected:interactive", "tiselected:viewer", "tiall:", "timail:"} {
		if seen[want] == 0 {
			t.Errorf("no row for %s in %v", want, seen)
		}
	}
	if seen["tistranger:"] != 0 {
		t.Errorf("a stranger saw rows: %v", seen)
	}
	removed := 0
	for key, rows := range seen {
		if strings.HasPrefix(key, "tiremoved:") {
			removed += rows
		}
	}
	if removed != 0 {
		t.Errorf("a removed member saw %d rows", removed)
	}
	// Past the end there is no row to carry the count; the service still reports it.
	ctx := workspace.WithContext(t.Context(), workspace.Scope{ID: "personal"})
	if _, total, err := f.service.TerminalSessionInventory(ctx, owner, InventoryOptions{Limit: 1, Offset: 400, WithoutLiveMetadata: true}); err != nil || total == 0 {
		t.Errorf("past the end of the personal list: total %d err %v", total, err)
	}
}
