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

// Migration 139 moves the team ENV grants into the team policy. Every member's ENV permission
// (write, read or none) must be the same after, in a team whose policy migration 128 wrote and
// in a team created later that has no policy yet.
func TestWorkspacePolicyENVMigrationKeepsAccess(t *testing.T) {
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
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 127); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	owner, admin, reader, writer, plain, revoked, auditor, gone := "own"+s, "adm"+s, "rea"+s, "wri"+s, "pla"+s, "rev"+s, "aud"+s, "gon"+s
	users := []string{owner, admin, reader, writer, plain, revoked, auditor, gone}
	for _, u := range users {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, u)
	}
	team, later := "t"+s, "l"+s
	addTeam := func(tm string) {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, tm, owner)
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true),($1,$5,1,'member',true),($1,$6,1,'member',true),($1,$7,1,'member',true),($1,$8,1,'auditor',true),($1,$9,1,'member',false)`,
			tm, owner, admin, reader, writer, plain, revoked, auditor, gone)
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'env',$1,$2)`, tm, owner)
		grant := func(account, permission string, active bool) {
			exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'env',$1,$3,1,$4)`, tm, account, permission, active)
		}
		grant(owner, "write", true)
		grant(reader, "read", true)
		grant(writer, "read", true)
		grant(writer, "write", true)
		grant(revoked, "read", false)
		grant(auditor, "read", true)
		grant(gone, "write", true)
	}
	addTeam(team)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 138); err != nil {
		t.Fatal(err)
	}
	addTeam(later) // created after migration 128: no policy row
	// The permission the vault used before (environment readVaultTeamTx).
	exec(`CREATE TEMP TABLE before_env AS SELECT m.team_id, m.account_id,
	  CASE WHEN t.owner_account=m.account_id OR m.role='admin' THEN 'write' ELSE COALESCE(g.permission,'read') END AS permission
	  FROM paperboat.team_members m JOIN paperboat.teams t USING(team_id)
	  LEFT JOIN LATERAL (SELECT CASE WHEN bool_or(permission='write') THEN 'write' WHEN bool_or(permission='read') THEN 'read' ELSE '' END AS permission
	    FROM paperboat.team_resource_grants WHERE team_id=m.team_id AND account_id=m.account_id AND resource_kind='env' AND resource_id=m.team_id AND active) g ON true
	  WHERE m.active AND m.team_id = ANY($1)`, []string{team, later})
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 139); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SQL().QueryContext(ctx, `SELECT b.team_id,b.account_id,b.permission,paperboat.team_env_permission(b.account_id,b.team_id) FROM before_env b ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var tm, account, before, after string
		if err := rows.Scan(&tm, &account, &before, &after); err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Errorf("%s %s: ENV permission %q before, %q after", tm, account, before, after)
		}
		n++
	}
	if n != 14 {
		t.Fatalf("compared %d member answers, want 14", n)
	}
	for _, tm := range []string{team, later} {
		var source, digest, message string
		var version int
		if err := store.SQL().QueryRowContext(ctx, `SELECT source,digest,version,message FROM paperboat.workspace_policies WHERE team_id=$1`, tm).Scan(&source, &digest, &version, &message); err != nil {
			t.Fatal(err)
		}
		if _, errs := policy.Parse([]byte(source)); len(errs) > 0 || !strings.Contains(source, "user:#"+writer) || strings.Contains(source, "user:#"+revoked) {
			t.Fatalf("%s policy v%d %q: %v\n%s", tm, version, message, errs, source)
		}
		var versions int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.workspace_policy_versions v JOIN paperboat.workspace_policies w ON w.id=v.policy_id WHERE w.team_id=$1 AND v.version=$2 AND v.digest=$3 AND v.kind='migrated'`, tm, version, digest).Scan(&versions); err != nil || versions != 1 {
			t.Fatalf("%s: the new version is not in the history (%d, %v)", tm, versions, err)
		}
	}
}
