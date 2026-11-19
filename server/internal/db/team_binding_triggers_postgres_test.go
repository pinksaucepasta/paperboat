package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// ceilingTeam is a team at its plan ceiling: 20 machine bindings among 12,001 bindings (2,000 ended terminal sessions,
// 9,981 previews), and a session of the team for each machine, none granted, so any change of the team's bindings
// revokes them.
type ceilingTeam struct {
	t           *testing.T
	ctx         context.Context
	store       *DB
	user, team  string
	exec        func(query string, args ...any)
	value       func(query string, args ...any) int64
	addSessions func(round string)
	revoked     func() int64
}

func newCeilingTeam(t *testing.T) ceilingTeam {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user, team, cli := "tb_user_"+s, "tb-team-"+s, "tb_cli_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	value := func(query string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return v
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at)
VALUES($1,$2,'pb','laptop','cli','linux',ARRAY['machines:connect'],'active',now(),now())`, cli, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT $1||'_env_'||g,$1||'_env_'||g,$1 FROM generate_series(0,19) g`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT $1||'_m_'||g,$1,$1||'_env_'||g,'m-'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(0,19) g`, user)
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		for _, c := range []struct {
			query string
			arg   string
		}{
			{`DELETE FROM paperboat.user_machine_access_sessions WHERE user_id=$1`, user}, {`DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, team},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, team}, {`DELETE FROM paperboat.user_machines WHERE user_id=$1`, user},
			{`DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`, user}, {`DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`, user},
			{`DELETE FROM paperboat.users WHERE id=$1`, user},
		} {
			if _, err := store.SQL().ExecContext(clean, c.query, c.arg); err != nil {
				t.Error(err)
			}
		}
	})
	// 20 machine bindings (they publish topics) among 12,001 bindings; 2,000 are ended terminal sessions.
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'machine',$2||'_m_'||g,$2,true,1 FROM generate_series(0,19) g`, team, user)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'terminal_session','ended_'||g,$2,false,1 FROM generate_series(1,2000) g`, team, user)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
SELECT $1,'preview','gone_'||g,$2,true,1 FROM generate_series(1,9981) g`, team, user)
	// A session of the team for each machine, none granted: a change to the team's bindings revokes all of them, once.
	addSessions := func(round string) {
		t.Helper()
		exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,team_id,capabilities,operation_id)
SELECT 'control_plane_signed','session_'||$4||$1||g,$1||'_m_'||g,$1,$1||'_env_'||g,$2,'https://invalid.test','term_'||$4||g,now()+interval '1 hour',$3,ARRAY['terminal'],'op_'||$4||$1||g FROM generate_series(0,19) g`, user, cli, team, round)
	}
	revoked := func() int64 {
		return value(`SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_id=$1 AND state='revoked' AND revocation_reason='machine_permission_revoked'`, user)
	}
	addSessions("u")
	return ceilingTeam{t: t, ctx: ctx, store: store, user: user, team: team, exec: exec, value: value, addSessions: addSessions, revoked: revoked}
}

// Deleting a team at its plan ceiling (12,001 bindings) ran three row-level triggers once per binding: 5.4 s unloaded,
// holding locks (migration 416). They now run once per statement. This fills a team to that size with real access
// sessions to revoke, times the bulk delete, and checks that no trigger on the table fires per row on a delete.
func TestCeilingSizedTeamBindingDeleteRunsStatementTriggersOnPostgres(t *testing.T) {
	c := newCeilingTeam(t)
	ctx, store, user, team, exec, value, addSessions, revoked := c.ctx, c.store, c.user, c.team, c.exec, c.value, c.addSessions, c.revoked
	if n := value(`SELECT count(*) FROM paperboat.team_resource_bindings WHERE team_id=$1`, team); n != 12001 {
		t.Fatalf("bindings = %d", n)
	}
	// The UPDATE path: one statement over all 12,001 bindings revokes the team's 20 sessions.
	exec(`UPDATE paperboat.team_resource_bindings SET generation=generation+1 WHERE team_id=$1 AND resource_kind='machine'`, team)
	if got := revoked(); got != 20 {
		t.Fatalf("revoked after a binding update = %d, want 20", got)
	}
	addSessions("d")
	topicBefore := value(`SELECT coalesce(sum(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id LIKE $1||'_m_%' AND topic='machine_access'`, user)

	started := time.Now()
	exec(`DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, team)
	elapsed := time.Since(started)
	t.Logf("deleted 12001 bindings in %v", elapsed)

	if got := revoked(); got != 40 {
		t.Fatalf("revoked after the delete = %d, want 40 (20 by the update, 20 by the delete)", got)
	}
	if moved := value(`SELECT coalesce(sum(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id LIKE $1||'_m_%' AND topic='machine_access'`, user) - topicBefore; moved < 20 {
		t.Fatalf("machine_access topics moved %d times, want at least one per machine (20)", moved)
	}
	// A sanity bound only (before migration 416 this delete took 3.4 s here, 5.4 s in the full team delete); the catalog
	// check below is the guard that a row-level trigger has not come back.
	if elapsed > 2*time.Second {
		t.Fatalf("deleting 12001 bindings took %v", elapsed)
	}
	var perRow int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger
WHERE tgrelid='paperboat.team_resource_bindings'::regclass AND NOT tgisinternal AND tgtype & 1 = 1 AND tgtype & 8 = 8`).Scan(&perRow); err != nil {
		t.Fatal(err)
	}
	if perRow != 0 {
		t.Fatalf("%d row-level triggers fire on a team_resource_bindings delete; they must be statement-level", perRow)
	}
}

// What production runs is not a delete: DeleteTeamTx and RemoveMemberTx deactivate bindings with one UPDATE
// (active=false) over the team's, or the member's, rows (teams/lifecycle.go revokeDepartingResourcesTx). At the plan
// ceiling that statement must stay fast and still revoke the sessions it invalidates.
func TestCeilingSizedTeamDeactivationRunsStatementTriggersOnPostgres(t *testing.T) {
	c := newCeilingTeam(t)
	const departing = `UPDATE paperboat.team_resource_bindings b SET active=false,generation=b.generation+1
WHERE b.team_id=$1 AND b.active AND ($2='' OR (b.owner_account=$2 AND b.resource_kind<>'env'
AND NOT (b.resource_kind='machine' AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.id=b.resource_id AND m.owner_team_id=$1))))`
	for i, step := range []struct{ name, account string }{{"member removal", c.user}, {"team deletion", ""}} {
		if i > 0 { // the first step revokes the fixture's own sessions
			c.addSessions(step.name)
		}
		before := c.revoked()
		started := time.Now()
		c.exec(departing, c.team, step.account)
		elapsed := time.Since(started)
		t.Logf("%s: deactivated %d bindings in %v", step.name, c.value(`SELECT count(*) FROM paperboat.team_resource_bindings WHERE team_id=$1 AND NOT active`, c.team), elapsed)
		if got := c.revoked() - before; got != 20 {
			t.Fatalf("%s revoked %d sessions, want 20", step.name, got)
		}
		if elapsed > time.Second {
			t.Fatalf("%s at the plan ceiling took %v", step.name, elapsed)
		}
		// Back to active for the next step.
		c.exec(`UPDATE paperboat.team_resource_bindings SET active=true WHERE team_id=$1 AND resource_kind<>'terminal_session'`, c.team)
	}
}
