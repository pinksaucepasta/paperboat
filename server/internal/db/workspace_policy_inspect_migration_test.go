package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/policy"
	"github.com/pressly/goose/v3"
)

// Migration 140 moves preview/tunnel inspect and replay grants into the team policy. For every
// member who does not own the resource, every resource and both actions, the answer must be
// the same after: an active grant of an active member on a bound resource, and nothing else.
func TestWorkspacePolicyInspectMigrationKeepsAccess(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_MIGRATION_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("empty isolated migration PostgreSQL DSN required")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// The migration database is shared with the other migration tests: start and leave it empty.
	exec(`DROP SCHEMA IF EXISTS paperboat CASCADE`)
	exec(`CREATE SCHEMA paperboat`)
	defer func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DROP SCHEMA IF EXISTS paperboat CASCADE`)
	}() // runs before store.Close
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 139); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	owner, both, inspector, none, gone := "own"+s, "bot"+s, "ins"+s, "non"+s, "gon"+s
	for _, u := range []string{owner, both, inspector, none, gone} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, u)
	}
	team := "t" + s
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true),($1,$4,1,'member',true),($1,$5,1,'member',true),($1,$6,1,'member',false)`, team, owner, both, inspector, none, gone)
	exec(`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'{"rules": []}','sha256:x')`, team)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id) VALUES($1,$2,$1,'host','linux','amd64','/w','online','occupied',true,$3)`, "m"+s, owner, team)
	tunnels := []string{"a" + s, "b" + s, "u" + s} // a and b bound to the team, u's binding inactive
	for i, tn := range tunnels {
		hex := fmt.Sprintf("%011d%d", time.Now().UnixNano()%1e11, i)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$1,'active','team',1,$3,$4,$5,$2,now(),now())`,
			tn, owner, "endpoint_00000000-0000-4000-8000-"+hex, "https://00000000-0000-8000-8000-"+hex+".tunnels.example.test", "m"+s)
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'tunnel',$2,$3,$4)`, team, tn, owner, i < 2)
	}
	grant := func(account, tn, permission string, active bool) {
		exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'tunnel',$3,$4,1,$5)`, team, account, tn, permission, active)
	}
	grant(both, tunnels[0], "inspect", true)
	grant(both, tunnels[0], "replay", true)
	grant(inspector, tunnels[0], "inspect", true)
	grant(inspector, tunnels[1], "replay", false) // inactive
	grant(inspector, tunnels[1], "use", true)     // use never implies inspect
	grant(none, tunnels[2], "inspect", true)      // resource no longer bound
	grant(gone, tunnels[0], "inspect", true)      // departed member
	exec(`CREATE TEMP TABLE before_inspect AS SELECT a.id AS actor, tn, action, EXISTS(
	    SELECT 1 FROM paperboat.team_resource_grants g JOIN paperboat.team_members m ON m.team_id=g.team_id AND m.account_id=g.account_id AND m.active
	    JOIN paperboat.team_resource_bindings b ON b.team_id=g.team_id AND b.resource_kind='tunnel' AND b.resource_id=g.resource_id AND b.active
	    WHERE g.team_id=$1 AND g.account_id=a.id AND g.resource_kind='tunnel' AND g.resource_id=tn AND g.permission=action AND g.active) AS allowed
	  FROM paperboat.users a CROSS JOIN unnest($2::text[]) tn CROSS JOIN unnest(ARRAY['inspect','replay']) action WHERE a.id=ANY($3)`,
		team, tunnels, []string{both, inspector, none, gone})
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 140); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SQL().QueryContext(ctx, `SELECT actor,tn,action,allowed,paperboat.workspace_policy_resource_allowed(actor,$1,'tunnel',tn,action) FROM before_inspect ORDER BY 1,2,3`, team)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n, allowed := 0, 0
	for rows.Next() {
		var actor, tn, action string
		var before, after bool
		if err := rows.Scan(&actor, &tn, &action, &before, &after); err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Errorf("%s %s %s: %v before, %v after", actor, tn, action, before, after)
		}
		n++
		if after {
			allowed++
		}
	}
	if n != 24 || allowed != 3 {
		t.Fatalf("compared %d answers (%d allowed), want 24 (3 allowed)", n, allowed)
	}
	var source string
	var version int
	if err := store.SQL().QueryRowContext(ctx, `SELECT source,version FROM paperboat.workspace_policies WHERE team_id=$1`, team).Scan(&source, &version); err != nil {
		t.Fatal(err)
	}
	if _, errs := policy.Parse([]byte(source)); len(errs) > 0 || version != 2 || !strings.Contains(source, "tunnel:#"+tunnels[0]) {
		t.Fatalf("policy v%d: %v\n%s", version, errs, source)
	}
}
