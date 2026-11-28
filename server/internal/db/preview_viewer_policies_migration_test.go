package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pressly/goose/v3"
)

// Migration 533 and its rollback keep every saved viewer policy exactly as it was.
func TestNoChangeSavedViewerPoliciesSurviveMigration533OnPostgres(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("requires empty isolated database: exists=%v error=%v", exists, err)
	}
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
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 532); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('usr_vp533','usr_vp533','vp533@test.invalid','active')`)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES('team-vp533','usr_vp533',1)`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES('mch_vp533','usr_vp533','env_vp533','linux','amd64','/workspace','online','occupied')`)
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at) VALUES('tun_vp533','usr_vp533','vp533','active','public',1,'endpoint_00000000-0000-4000-8000-000000000533','https://00000000-0000-4000-8000-000000000533.tunnels.example.test','mch_vp533','usr_vp533','pending',now(),now())`)
	for i, row := range []struct{ audience, team, emails, domain string }{
		{audience: "public", emails: "{}"},
		{audience: "owner", emails: "{}"},
		{audience: "specific_emails", emails: "{b@example.test,a@example.test}"},
		{audience: "email_domain", emails: "{}", domain: "example.test"},
		{audience: "team", emails: "{}", team: "team-vp533"},
	} {
		route := "rte_vp533_" + row.audience
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,'tun_vp533',$2,'http','managed','00000000-0000-4000-8000-000000000533.tunnels.example.test',$3,$4,'http','127.0.0.1:3400','active',1,'usr_vp533','usr_vp533',now(),now())`, route, row.audience, "/"+row.audience, 100+i)
		exec(`INSERT INTO paperboat.browser_viewer_policies(resource_kind,resource_id,route_id,owner_account_id,audience,team_id,allowed_emails,email_domain,generation,updated_by_account) VALUES('tunnel','tun_vp533',$1,'usr_vp533',$2,NULLIF($3,''),$4::text[],NULLIF($5,''),$6,'usr_vp533')`, route, row.audience, row.team, row.emails, row.domain, i+1)
	}
	rows := func() (saved string) {
		t.Helper()
		var count int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*),jsonb_agg(to_jsonb(p) ORDER BY p.route_id)::text FROM paperboat.browser_viewer_policies p`).Scan(&count, &saved); err != nil || count != 5 {
			t.Fatalf("saved policies = %d, %v", count, err)
		}
		return saved
	}
	before := rows()
	for _, step := range []struct {
		name    string
		migrate func() error
	}{
		{"up to 533", func() error { return goose.UpToContext(ctx, store.SQL(), migrationsDir, 533) }},
		{"down to 532", func() error { return goose.DownToContext(ctx, store.SQL(), migrationsDir, 532) }},
		{"up to 533 again", func() error { return goose.UpToContext(ctx, store.SQL(), migrationsDir, 533) }},
	} {
		if err = step.migrate(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if after := rows(); after != before {
			t.Fatalf("%s changed the saved policies:\n before %s\n after  %s", step.name, before, after)
		}
	}
	// A deleted route still takes its policy with it, as the dropped foreign key did.
	exec(`DELETE FROM paperboat.tunnel_routes WHERE id='rte_vp533_owner'`)
	var left int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.browser_viewer_policies`).Scan(&left); err != nil || left != 4 {
		t.Fatalf("policies after a route delete = %d, %v", left, err)
	}
}
