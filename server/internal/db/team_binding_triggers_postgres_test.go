package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Deleting a team at its plan ceiling (12,001 bindings) ran three row-level triggers once per binding: 5.4 s unloaded,
// holding locks (migration 416). They now run once per statement. This fills a team to that size with real access
// sessions to revoke, times the bulk delete, and checks that no trigger on the table fires per row on a delete.
func TestCeilingSizedTeamBindingDeleteRunsStatementTriggersOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
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
SELECT 'external_issuer','session_'||$4||$1||g,$1||'_m_'||g,$1,$1||'_env_'||g,$2,'https://invalid.test','term_'||$4||g,now()+interval '1 hour',$3,ARRAY['terminal'],'op_'||$4||$1||g FROM generate_series(0,19) g`, user, cli, team, round)
	}
	revoked := func() int64 {
		return value(`SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_id=$1 AND state='revoked' AND revocation_reason='machine_permission_revoked'`, user)
	}
	addSessions("u")
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
