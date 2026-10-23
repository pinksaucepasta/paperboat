package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/policy"
	"github.com/pressly/goose/v3"
)

// Migration 128 turns team machine grants and allow_private_access into policy version 1. Every
// member × machine × capability answer, and private access per port, must be the same after.
func TestWorkspacePolicyMigrationKeepsAccess(t *testing.T) {
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
	exec(`CREATE SCHEMA paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 127); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	owner, admin, member, other, gone := "own"+s, "adm"+s, "mem"+s, "oth"+s, "gon"+s
	for _, u := range []string{owner, admin, member, other, gone} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, u)
	}
	team, quiet := "t"+s, "q"+s
	for _, tm := range []string{team, quiet} {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, tm, owner)
		exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',4,now()-interval '1 hour',now()+interval '1 hour')`, "bw"+tm, tm)
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'member',true),($1,$5,1,'member',true),($1,$6,1,'member',false)`, tm, owner, admin, member, other, gone)
	}
	exec(`UPDATE paperboat.teams SET allow_private_access=true WHERE team_id=$1`, team)
	// db (team-owned), laptop twice (shared personal machines with the same alias), spare (no grant).
	machines := map[string][2]string{"db" + s: {owner, team}, "la" + s: {member, ""}, "lb" + s: {other, ""}, "sp" + s: {admin, team}}
	for id, m := range machines {
		alias := map[byte]string{'d': "db", 'l': "laptop", 's': "spare"}[id[0]]
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id) VALUES($1,$2,$1,$3,'linux','amd64','/w','online','occupied',true,nullif($4,''))`, id, m[0], alias, m[1])
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, id, m[0])
	}
	grant := func(tm, machine, account string, active bool, caps ...string) {
		audience := "all_members"
		if account != "" {
			audience = "selected_member"
		}
		exec(`INSERT INTO paperboat.team_machine_grants(team_id,machine_id,audience,account_id,capabilities,active) VALUES($1,$2,$3,nullif($4,''),$5,$6)`, tm, machine, audience, account, caps, active)
	}
	grant(team, "db"+s, "", true, "terminal", "exec")
	grant(team, "db"+s, member, true, "private_access", "files")
	grant(team, "la"+s, other, true, "managed_ssh", "private_access")
	grant(team, "lb"+s, "", true, "preview_manage")
	grant(team, "lb"+s, gone, true, "terminal")    // departed member: allows nothing today
	grant(team, "sp"+s, member, false, "terminal") // inactive grant
	exec(`CREATE TEMP TABLE before_access AS SELECT a.id AS actor, m.id AS machine, cap, paperboat.team_machine_capability_allowed(a.id,$1,m.id,cap) AS allowed
FROM paperboat.users a CROSS JOIN paperboat.user_machines m CROSS JOIN unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage','private_access']) cap WHERE a.id LIKE '%'||$2`, team, s)
	exec(`CREATE TEMP TABLE before_ports AS SELECT a.id AS actor, m.id AS machine, port, EXISTS(SELECT 1 FROM paperboat.machine_service_authorities(a.id,m.id,port) WHERE source_kind='automatic' AND team_id=$1) AS allowed
FROM paperboat.users a CROSS JOIN paperboat.user_machines m CROSS JOIN unnest(ARRAY[22,1023,1024,5432,65535]) port WHERE a.id LIKE '%'||$2`, team, s)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 128); err != nil {
		t.Fatal(err)
	}
	var changed []string
	rows, err := store.SQL().QueryContext(ctx, `SELECT actor||' '||machine||' '||cap||' '||allowed FROM before_access WHERE allowed IS DISTINCT FROM paperboat.team_machine_capability_allowed(actor,$1,machine,cap)
UNION ALL SELECT actor||' '||machine||' port '||port||' '||allowed FROM before_ports WHERE allowed IS DISTINCT FROM EXISTS(SELECT 1 FROM paperboat.machine_service_authorities(actor,machine,port) WHERE source_kind='automatic' AND team_id=$1)`, team)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		changed = append(changed, line)
	}
	rows.Close()
	if len(changed) > 0 {
		t.Fatalf("access changed by the migration (before value shown):\n%s", strings.Join(changed, "\n"))
	}
	var allowedBefore int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE allowed) FROM before_access`).Scan(&allowedBefore); err != nil || allowedBefore < 8 {
		t.Fatalf("fixture must allow something: %d %v", allowedBefore, err)
	}
	for _, tm := range []string{team, quiet} {
		var source string
		var version int
		var private bool
		if err = store.SQL().QueryRowContext(ctx, `SELECT source,version,allow_private_access FROM paperboat.workspace_policies WHERE team_id=$1`, tm).Scan(&source, &version, &private); err != nil {
			t.Fatal(err)
		}
		d, errs := policy.Parse([]byte(source))
		if len(errs) > 0 || version != 1 || private != (tm == team) {
			t.Fatalf("%s v%d private=%v %v\n%s", tm, version, private, errs, source)
		}
		if tm == team && (len(d.Rules) != 4 || !strings.Contains(source, `"machine:#db`+s+`"`) || !strings.Contains(source, `"user:#`+member+`"`) || strings.Contains(source, gone)) {
			t.Fatalf("version 1 rules:\n%s", source)
		}
		if tm == quiet && len(d.Rules) != 0 {
			t.Fatalf("a team without grants gets an empty policy:\n%s", source)
		}
	}
}
