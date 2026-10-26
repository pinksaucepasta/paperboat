package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// The preview and tunnel lists take their candidates from the hosts an account may manage publications on (own
// personal machines, team machines whose workspace policy gives it preview_manage/tunnel_manage) and decide them with
// each host check computed once per host (migration 174). For every actor, workspace and machine filter the lists
// must equal what the access functions alone allow, and their counts the lists' length.
func TestPublicationListCandidatesMatchTheAccessRulesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	f := newPublicationFixture(t, ctx, store)
	s, team, users, machines := f.s, f.team, f.users, f.machines
	reference := func(kind, actor string, set bool, teamID sql.NullString, machine string) []string {
		query := `SELECT id FROM paperboat.preview_leases WHERE id LIKE 'pc_preview_%_'||$4 AND paperboat.preview_management_allowed($1,id)
 AND (NOT $2 OR paperboat.workspace_preview_management_allowed($1,$3,id)) AND ($5='' OR owner_machine_id=$5) ORDER BY id`
		if kind == "tunnel" {
			query = `SELECT id FROM paperboat.tunnels t WHERE id LIKE 'pc_tunnel_%_'||$4 AND desired_state<>'deleted' AND paperboat.tunnel_management_allowed($1,id)
 AND (NOT $2 OR paperboat.workspace_tunnel_management_allowed($1,$3,id))
 AND ($5='' OR created_by_host_id=$5) ORDER BY id`
		}
		rows, err := store.SQL().QueryContext(ctx, query, actor, set, teamID, s, machine)
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
		slices.Sort(ids)
		return ids
	}
	seen, seenOnMachine := 0, 0
	var deletedSeen int64
	for _, actor := range users {
		for _, scope := range []struct {
			set  bool
			team sql.NullString
		}{{false, sql.NullString{}}, {true, sql.NullString{}}, {true, sql.NullString{String: team, Valid: true}}} {
			previews, err := store.Queries().ListPreviewLeasesV1(ctx, dbsqlc.ListPreviewLeasesV1Params{AccountID: actor, RowLimit: 200, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s})
			if err != nil {
				t.Fatal(err)
			}
			var gotPreviews []string
			for _, row := range previews {
				gotPreviews = append(gotPreviews, row.ID)
			}
			tunnels, err := store.Queries().ListPreviewTunnelsV1(ctx, dbsqlc.ListPreviewTunnelsV1Params{AccountID: actor, RowLimit: 200, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s})
			if err != nil {
				t.Fatal(err)
			}
			var gotTunnels []string
			for _, row := range tunnels {
				gotTunnels = append(gotTunnels, row.ID)
			}
			slices.Sort(gotPreviews)
			slices.Sort(gotTunnels)
			if want := reference("preview", actor, scope.set, scope.team, ""); !slices.Equal(gotPreviews, want) {
				t.Fatalf("previews for %s set=%v team=%v: listed %v, access rules allow %v", actor, scope.set, scope.team.String, gotPreviews, want)
			}
			if want := reference("tunnel", actor, scope.set, scope.team, ""); !slices.Equal(gotTunnels, want) {
				t.Fatalf("tunnels for %s set=%v team=%v: listed %v, access rules allow %v", actor, scope.set, scope.team.String, gotTunnels, want)
			}
			counted := func(machine string) (previews, tunnels int64) {
				t.Helper()
				previews, err := store.Queries().CountPreviewLeasesV1(ctx, dbsqlc.CountPreviewLeasesV1Params{AccountID: actor, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, MachineID: machine})
				if err != nil {
					t.Fatal(err)
				}
				tunnels, err = store.Queries().CountPreviewTunnelsV1(ctx, dbsqlc.CountPreviewTunnelsV1Params{AccountID: actor, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, MachineID: machine})
				if err != nil {
					t.Fatal(err)
				}
				return previews, tunnels
			}
			// Walking the cursor in pages of 3 lists exactly the full list (the lists stop deciding at the page).
			var walkedPreviews, walkedTunnels []string
			for after := (struct {
				at sql.NullTime
				id sql.NullString
			}{}); ; {
				page, err := store.Queries().ListPreviewLeasesV1(ctx, dbsqlc.ListPreviewLeasesV1Params{AccountID: actor, RowLimit: 3, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, AfterCreatedAt: after.at, AfterID: after.id})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range page {
					walkedPreviews = append(walkedPreviews, row.ID)
				}
				if len(page) < 3 {
					break
				}
				last := page[len(page)-1]
				after.at, after.id = sql.NullTime{Time: last.CreatedAt, Valid: true}, sql.NullString{String: last.ID, Valid: true}
			}
			for after := (struct {
				at sql.NullTime
				id sql.NullString
			}{}); ; {
				page, err := store.Queries().ListPreviewTunnelsV1(ctx, dbsqlc.ListPreviewTunnelsV1Params{AccountID: actor, RowLimit: 3, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, AfterCreatedAt: after.at, AfterID: after.id})
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range page {
					walkedTunnels = append(walkedTunnels, row.ID)
				}
				if len(page) < 3 {
					break
				}
				last := page[len(page)-1]
				after.at, after.id = sql.NullTime{Time: last.CreatedAt, Valid: true}, sql.NullString{String: last.ID, Valid: true}
			}
			slices.Sort(walkedPreviews)
			slices.Sort(walkedTunnels)
			if !slices.Equal(walkedPreviews, gotPreviews) || !slices.Equal(walkedTunnels, gotTunnels) {
				t.Fatalf("paging for %s set=%v team=%v: walked %v and %v, full lists %v and %v", actor, scope.set, scope.team.String, walkedPreviews, walkedTunnels, gotPreviews, gotTunnels)
			}
			// Live and Ended split the previews exactly; deleted tunnels are listed only when asked for.
			stateCount := func(kind, state string) int64 {
				t.Helper()
				var n int64
				var err error
				if kind == "preview" {
					n, err = store.Queries().CountPreviewLeasesV1(ctx, dbsqlc.CountPreviewLeasesV1Params{AccountID: actor, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, State: state})
				} else {
					n, err = store.Queries().CountPreviewTunnelsV1(ctx, dbsqlc.CountPreviewTunnelsV1Params{AccountID: actor, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, State: state})
				}
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			if active, ended, all := stateCount("preview", "active"), stateCount("preview", "ended"), stateCount("preview", ""); active+ended != all {
				t.Fatalf("previews for %s: %d active + %d ended != %d", actor, active, ended, all)
			}
			deleted := 0
			for _, id := range gotTunnels {
				if id == fmt.Sprintf("pc_tunnel_8_%s", s) {
					deleted++
				}
			}
			if deleted != 0 {
				t.Fatalf("the default tunnel list of %s holds the deleted tunnel", actor)
			}
			deletedSeen += stateCount("tunnel", "deleted")
			// The totals count exactly what the lists page through.
			if previews, tunnels := counted(""); previews != int64(len(gotPreviews)) || tunnels != int64(len(gotTunnels)) {
				t.Fatalf("counts for %s set=%v team=%v: %d previews, %d tunnels; listed %d and %d", actor, scope.set, scope.team.String, previews, tunnels, len(gotPreviews), len(gotTunnels))
			}
			// One machine's publications (machine_id): the same rules narrowed to that machine.
			for _, machine := range machines {
				previews, err := store.Queries().ListPreviewLeasesV1(ctx, dbsqlc.ListPreviewLeasesV1Params{AccountID: actor, RowLimit: 200, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, MachineID: machine})
				if err != nil {
					t.Fatal(err)
				}
				tunnels, err := store.Queries().ListPreviewTunnelsV1(ctx, dbsqlc.ListPreviewTunnelsV1Params{AccountID: actor, RowLimit: 200, WorkspaceSet: scope.set, WorkspaceTeamID: scope.team, Q: s, MachineID: machine})
				if err != nil {
					t.Fatal(err)
				}
				var onPreviews, onTunnels []string
				for _, row := range previews {
					onPreviews = append(onPreviews, row.ID)
				}
				for _, row := range tunnels {
					onTunnels = append(onTunnels, row.ID)
				}
				slices.Sort(onPreviews)
				slices.Sort(onTunnels)
				if want := reference("preview", actor, scope.set, scope.team, machine); !slices.Equal(onPreviews, want) {
					t.Fatalf("previews of %s for %s: listed %v, want %v", machine, actor, onPreviews, want)
				}
				if want := reference("tunnel", actor, scope.set, scope.team, machine); !slices.Equal(onTunnels, want) {
					t.Fatalf("tunnels of %s for %s: listed %v, want %v", machine, actor, onTunnels, want)
				}
				if previews, tunnels := counted(machine); previews != int64(len(onPreviews)) || tunnels != int64(len(onTunnels)) {
					t.Fatalf("counts of %s for %s: %d previews, %d tunnels; listed %d and %d", machine, actor, previews, tunnels, len(onPreviews), len(onTunnels))
				}
				seenOnMachine += min(len(onPreviews)+len(onTunnels), 1)
			}
			seen += min(len(gotPreviews), 1) + min(len(gotTunnels), 1)
		}
	}
	if deletedSeen == 0 {
		t.Fatal("no actor lists the deleted tunnel with state=deleted")
	}
	if seen < 12 || seenOnMachine < 8 {
		t.Fatalf("fixture too weak: only %d non-empty lists, %d non-empty machine lists", seen, seenOnMachine)
	}
}

// publicationFixture is a team with every shape of publication access: owner, admin, members and an outsider;
// team machines with per-account, role, group-on-tag, every-machine and deny rules; personal machines, a machine
// shared into the team, a revoked host; previews and tunnels on each host in every access mode, owned personally,
// by the team or shared by binding; tunnel connectors on hosts with and without tunnel_manage, a revoked connector
// and a deleted tunnel.
type publicationFixture struct {
	s, team         string
	users, machines []string
}

func newPublicationFixture(t *testing.T, ctx context.Context, store *DB) publicationFixture {
	t.Helper()
	s := fmt.Sprint(time.Now().UnixNano())
	owner, admin, member, picked, outsider := "pc_owner_"+s, "pc_admin_"+s, "pc_member_"+s, "pc_picked_"+s, "pc_outsider_"+s
	users := []string{owner, admin, member, picked, outsider}
	team := "pc-team-" + s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@example.test','active' FROM unnest($1::text[]) x`, users)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team',5,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES
($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true),($1,$5,1,'member',true)`, team, owner, admin, member, picked)
	machine := func(name, user string, ownerTeam any) string {
		id := "pc_machine_" + name + "_" + s
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id,
configured_capabilities,observed_capabilities) VALUES($1,$2,$1,$3,'linux','amd64','/w','online','occupied',true,$4,ARRAY['preview_launch','terminal_host'],ARRAY['preview_launch'])`,
			id, user, "pc-"+strings.ReplaceAll(name, "_", "-"), ownerTeam)
		if ownerTeam != nil {
			exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, team, id, user)
		}
		return id
	}
	// Machine access is the team's workspace policy (migration 128): a rule per machine for every member or one.
	exec(`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'{}','sha256:test')`, team)
	grant := func(machine string, account any, capabilities ...string) {
		kind, subject := "role", any("member")
		if account != nil {
			kind, subject = "account", account
		}
		exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
SELECT p.id,0,false,$2,$3,'machine',$4,c,int4range(1,65536) FROM paperboat.workspace_policies p,unnest($5::text[]) c WHERE p.team_id=$1`,
			team, kind, subject, machine, capabilities)
	}
	teamAll, teamPicked, teamNone := machine("team_all", admin, team), machine("team_picked", owner, team), machine("team_none", member, team)
	memberOwn, outsiderOwn := machine("member_own", member, nil), machine("outsider_own", outsider, nil)
	shared := machine("shared", outsider, nil)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'machine',$2,$3,true,1)`, team, shared, outsider)
	grant(teamAll, nil, "preview_manage", "tunnel_manage", "terminal")
	grant(teamPicked, picked, "preview_manage")
	grant(teamNone, nil, "terminal")
	grant(shared, nil, "tunnel_manage", "preview_manage")
	// The other rule shapes: a group on a tag, a role on every machine, and a deny that wins over an allow.
	rule := func(deny bool, subjectKind, subject, targetKind, target, action string) {
		exec(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports)
SELECT id,1,$2,$3,$4,$5,$6,$7,int4range(1,65536) FROM paperboat.workspace_policies WHERE team_id=$1`, team, deny, subjectKind, subject, targetKind, target, action)
	}
	exec(`INSERT INTO paperboat.workspace_policy_tags(policy_id,tag,machine_id) SELECT id,'build',$2 FROM paperboat.workspace_policies WHERE team_id=$1`, team, teamNone)
	exec(`INSERT INTO paperboat.workspace_policy_groups(policy_id,group_name,account_id) SELECT id,'ops',$2 FROM paperboat.workspace_policies WHERE team_id=$1`, team, picked)
	rule(false, "group", "ops", "tag", "build", "preview_manage")
	rule(false, "role", "admin", "all_machines", "", "tunnel_manage")
	rule(true, "account", member, "machine", teamAll, "preview_manage")

	type publication struct {
		host, account, mode string
		team                any
	}
	var publications []publication
	for _, host := range []struct {
		id, account string
		team        any
	}{{teamAll, admin, team}, {teamPicked, owner, team}, {teamNone, member, team}, {memberOwn, member, nil}, {outsiderOwn, outsider, nil}, {shared, outsider, nil}, {shared, outsider, team}} {
		for _, mode := range []string{"public", "private", "team"} {
			publications = append(publications, publication{host.id, host.account, mode, host.team})
		}
	}
	for i, p := range publications {
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,
lease_deadline,owner_session_kind,owner_team_id) VALUES($1,$2,$3,$3,$4,'session','http','127.0.0.1:3000',$5,now()+interval '1 hour','local_lease',$6)`,
			fmt.Sprintf("pc_preview_%d_%s", i, s), fmt.Sprintf("https://pc%d-%s.example.test", i, s), p.account, p.host, p.mode, p.team)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,access_mode,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,owner_team_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$2,$8)`, fmt.Sprintf("pc_tunnel_%d_%s", i, s), p.account, fmt.Sprintf("pc-%d", i), p.mode, "endpoint_"+uuid.NewString(),
			fmt.Sprintf("https://pct%d-%s.example.test", i, s), p.host, p.team)
	}
	// A personal preview and tunnel shared to the team by binding.
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES
($1,'preview',$2,$3,true,1),($1,'tunnel',$4,$3,true,1)`, team, fmt.Sprintf("pc_preview_%d_%s", 17, s), outsider, fmt.Sprintf("pc_tunnel_%d_%s", 17, s))

	revoked := machine("revoked", admin, team)
	grant(revoked, nil, "preview_manage", "tunnel_manage")
	for i, mode := range []string{"public", "private", "team"} {
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,
lease_deadline,owner_session_kind,owner_team_id) VALUES($1,$2,$3,$3,$4,'session','http','127.0.0.1:3000',$5,now()+interval '1 hour','local_lease',$6)`,
			fmt.Sprintf("pc_preview_r%d_%s", i, s), fmt.Sprintf("https://pcr%d-%s.example.test", i, s), admin, revoked, mode, team)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,access_mode,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,owner_team_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$2,$8)`, fmt.Sprintf("pc_tunnel_r%d_%s", i, s), admin, fmt.Sprintf("pc-r%d", i), mode, "endpoint_"+uuid.NewString(),
			fmt.Sprintf("https://pctr%d-%s.example.test", i, s), revoked, team)
	}
	exec(`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, revoked)
	exec(`UPDATE paperboat.tunnels SET deleted_at=now(),desired_state='deleted' WHERE id=$1`, fmt.Sprintf("pc_tunnel_%d_%s", 8, s))
	var machines []string
	rows, err := store.SQL().QueryContext(ctx, `SELECT id FROM paperboat.user_machines WHERE id LIKE 'pc_machine_%_'||$1 ORDER BY id`, s)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		machines = append(machines, id)
	}
	rows.Close()
	return publicationFixture{s: s, team: team, users: users, machines: machines}
}
