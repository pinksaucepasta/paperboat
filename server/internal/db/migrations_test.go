package db_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pressly/goose/v3"
)

// latestMigrationVersion is the highest migration number on disk. Version pins
// derive from it so that adding a migration does not require editing the tests.
func latestMigrationVersion(t *testing.T) int64 {
	t.Helper()
	names, err := fs.Glob(os.DirFS("migrations"), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var latest int64
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %s has no numeric version: %v", name, err)
		}
		latest = max(latest, version)
	}
	if latest == 0 {
		t.Fatal("no migrations found")
	}
	return latest
}

func TestBaselineSchema(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT max(version_id) FROM paperboat.goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if latest := latestMigrationVersion(t); version != latest {
		t.Fatalf("schema version = %d, want latest migration %d", version, latest)
	}
	for _, object := range []string{"users", "teams", "user_machines", "peer_endpoint_certificates", "tunnels", "tunnel_config_generations", "environment_password_vaults", "selfhost_installations", "selfhost_pool_policies", "selfhost_placements"} {
		var exists bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.'||$1) IS NOT NULL`, object).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("missing baseline table %s", object)
		}
	}
	for _, retired := range []string{"polar_events", "peer_session_intents", "peer_signaling_grants", "peer_relay_allocations", "fly_machines", "selfhost_challenges", "selfhost_pairings", "selfhost_pairing_codes", "selfhost_operator_nonces"} {
		var exists bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.'||$1) IS NOT NULL`, retired).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("retired table remains: %s", retired)
		}
	}
	for _, column := range []struct{ table, name string }{{"selfhost_installations", "installation_public_key"}, {"selfhost_installations", "scope_kind"}, {"selfhost_installations", "scope_id"}, {"selfhost_installations", "claim_id"}, {"selfhost_installations", "claim_complete"}, {"selfhost_pool_policies", "scope_kind"}, {"selfhost_pool_policies", "scope_id"}, {"control_tunnel_nodes", "tls_spki_sha256"}, {"support_reports", "operation"}, {"support_reports", "correlation_id"}, {"support_reports", "cause"}, {"support_reports", "error_type"}, {"support_reports", "error_chain"}, {"support_reports", "errno"}, {"support_reports", "http_status"}, {"support_reports", "outcome"}, {"support_reports", "severity"}} {
		var exists bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='paperboat' AND table_name=$1 AND column_name=$2)`, column.table, column.name).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing scoped column %s.%s: %v", column.table, column.name, err)
		}
	}
	for _, column := range []struct{ table, name string }{{"user_machines", "setup_mode"}, {"user_machines", "setup_roles"}, {"user_machine_pairings", "authenticated_setup_mode"}, {"config_compare_admissions", "browser_session_id"}, {"config_compare_admissions", "browser_key_sha256"}, {"config_compare_admissions", "ticket_hash"}, {"config_compare_admissions", "ticket_consumed_at"}, {"config_compare_admissions", "origin"}, {"config_compare_admissions", "runtime_host"}, {"config_compare_admissions", "edge_node_id"}, {"config_compare_admissions", "edge_process_epoch"}, {"selfhost_installations", "operator_public_key"}, {"selfhost_pool_policies", "account_id"}} {
		var exists bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='paperboat' AND table_name=$1 AND column_name=$2)`, column.table, column.name).Scan(&exists); err != nil || exists {
			t.Fatalf("retired column %s.%s remains: %v", column.table, column.name, err)
		}
	}
	for _, index := range []string{"user_machine_access_sessions_revocation_expiry", "cli_refresh_tokens_session", "user_machine_terminal_session_operations_pending_created"} {
		var exists bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.'||$1) IS NOT NULL`, index).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("missing workload index %s", index)
		}
	}
	var extension bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='pgcrypto' AND extnamespace='paperboat'::regnamespace)`).Scan(&extension); err != nil {
		t.Fatal(err)
	}
	if extension {
		t.Fatal("unused pgcrypto extension remains in fresh schema")
	}
	if _, err := store.SQL().ExecContext(ctx, `SELECT paperboat.revoke_config_sync_for_environment('migration_probe')`); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentMigrateCallsAreSerialized(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run Postgres migration integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	start := make(chan struct{})
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- db.Migrate(ctx, store)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate failed: %v", err)
		}
	}
}

func TestTransactionRollsBackOnError(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run Postgres transaction integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("force rollback")
	err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO users (id, workos_subject, primary_email, status)
VALUES ('user_rollback_test', 'workos_rollback_test', 'rollback@example.com', 'active')
ON CONFLICT (id) DO UPDATE SET primary_email = EXCLUDED.primary_email`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	var exists bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM paperboat.users WHERE id = 'user_rollback_test')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("rolled back transaction still inserted user")
	}
}

func TestTouchClientSessionThrottlesAndTouchesNullLastUsed(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run Postgres transaction integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	sessionID := "cli_session_touch_test"
	if _, err := store.SQL().Exec(`
INSERT INTO paperboat.users (id, workos_subject, primary_email, status)
VALUES ('user_touch_test', 'workos_touch_test', 'touch@example.com', 'active')
ON CONFLICT (id) DO UPDATE SET primary_email = EXCLUDED.primary_email`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().Exec(`
INSERT INTO paperboat.cli_client_sessions (id, user_id, client_id, client_label, device_type, os, scopes, state, created_at, approved_at, last_used_at)
VALUES ($1, 'user_touch_test', 'paperboat', 'test', 'desktop', 'darwin', ARRAY['machines:read'], 'active', now(), now(), NULL)
ON CONFLICT (id) DO UPDATE SET last_used_at = NULL`, sessionID); err != nil {
		t.Fatal(err)
	}
	queries := store.Queries()
	first := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if err := queries.TouchClientSession(ctx, dbsqlc.TouchClientSessionParams{ID: sessionID, LastUsedAt: sql.NullTime{Time: first, Valid: true}, TouchIntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	var lastUsed *time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT last_used_at FROM paperboat.cli_client_sessions WHERE id = $1`, sessionID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed == nil || !lastUsed.Equal(first) {
		t.Fatalf("first touch did not set last_used_at: %v", lastUsed)
	}
	second := first.Add(10 * time.Second)
	if err := queries.TouchClientSession(ctx, dbsqlc.TouchClientSessionParams{ID: sessionID, LastUsedAt: sql.NullTime{Time: second, Valid: true}, TouchIntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT last_used_at FROM paperboat.cli_client_sessions WHERE id = $1`, sessionID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed == nil || !lastUsed.Equal(first) {
		t.Fatalf("touch within the throttle interval rewrote last_used_at: %v", lastUsed)
	}
	third := first.Add(2 * time.Minute)
	if err := queries.TouchClientSession(ctx, dbsqlc.TouchClientSessionParams{ID: sessionID, LastUsedAt: sql.NullTime{Time: third, Valid: true}, TouchIntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT last_used_at FROM paperboat.cli_client_sessions WHERE id = $1`, sessionID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed == nil || !lastUsed.Equal(third) {
		t.Fatalf("touch after the throttle interval did not rewrite last_used_at: %v", lastUsed)
	}
}

// Exercise the deployed schema's populated upgrade, including deliberate
// capability retirement and preservation of existing Personal publications.
func TestPopulatedSchema45To61Upgrade(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_MIGRATION_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("empty isolated migration PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var occupied bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&occupied); err != nil || occupied {
		t.Fatalf("requires empty isolated database: occupied=%v error=%v", occupied, err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA paperboat`)
	goose.SetBaseFS(os.DirFS("."))
	goose.SetTableName("paperboat.goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, store.SQL(), "migrations", 45); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('upgrade-owner','upgrade-owner','upgrade@example.test','active')`)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES('upgrade-team','upgrade-owner',7)`)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES('upgrade-team','upgrade-owner',3,'admin',true)`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities,capabilities_desired_version) VALUES('upgrade-machine','upgrade-owner','upgrade-machine','linux','amd64','/workspace','online','occupied',true,ARRAY['preview_launch','peer_relay'],ARRAY['preview_launch','peer_relay'],4)`)
	exec(`INSERT INTO paperboat.account_network_preferences(account_id,revision,machine_suffix,selected_team_id) VALUES('upgrade-owner',2,'old.example','upgrade-team')`)
	exec(`INSERT INTO paperboat.team_network_preferences(team_id,revision,machine_suffix) VALUES('upgrade-team',3,'old-team.example')`)
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,access_mode,generation) VALUES('upgrade-tunnel','upgrade-owner','retained-name','endpoint_123e4567-e89b-42d3-a456-426614174000','https://123e4567-e89b-42d3-a456-426614174000.tunnels.example.test','upgrade-machine','upgrade-owner','private',5)`)
	exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline,generation) VALUES('upgrade-preview','https://upgrade.preview.example.test','upgrade-owner','upgrade-owner','upgrade-machine','upgrade-session','foreground','http','127.0.0.1:3000','private',now()+interval '1 hour',6)`)
	snapshot := func(upgraded bool) string {
		t.Helper()
		tunnel, preview, team := "to_jsonb(t)", "to_jsonb(p)", "to_jsonb(t)"
		if upgraded {
			team += "-'display_name'"
			tunnel += "-'owner_team_id'"
			preview += "-'owner_team_id'"
		}
		q := `SELECT jsonb_build_object('user',(SELECT to_jsonb(u) FROM paperboat.users u WHERE id='upgrade-owner'),'team',(SELECT ` + team + ` FROM paperboat.teams t WHERE team_id='upgrade-team'),'member',(SELECT to_jsonb(m) FROM paperboat.team_members m WHERE team_id='upgrade-team'),'tunnel',(SELECT ` + tunnel + ` FROM paperboat.tunnels t WHERE id='upgrade-tunnel'),'preview',(SELECT ` + preview + ` FROM paperboat.preview_leases p WHERE id='upgrade-preview'))::text`
		var value string
		if err := store.SQL().QueryRowContext(ctx, q).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot(false)
	// Seed both 057 admission variants before 061; only the browser ticket may be removed.
	if err := goose.UpToContext(ctx, store.SQL(), "migrations", 60); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES('upgrade-machine','upgrade-machine','upgrade-owner','active')`)
	exec(`INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name,state) VALUES('upgrade-repo','upgrade-owner','github','upgrade-repo','upgrade/config','active')`)
	exec(`INSERT INTO paperboat.control_config_assignments(id,machine_id,environment_id,repository_id,consent_state,warning_revision,accepted_at,configuration_revision) VALUES('upgrade-assignment','upgrade-machine','upgrade-machine','upgrade-repo','accepted','warning',now(),repeat('a',64))`)
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES('upgrade-browser-session','upgrade-owner','upgrade-browser-session','upgrade-browser-session',now()+interval '1 hour')`)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES('upgrade-cli-session','upgrade-owner','upgrade-cli','Migration','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`)
	exec(`INSERT INTO paperboat.config_compare_admissions(id,operation_id,account_id,environment_id,machine_id,installation_generation,assignment_id,assignment_version,config_path,conflict_revision,expected_remote_revision,native_cli_session_id,source_machine_id,workspace_id,expires_at) VALUES('upgrade-native-admission','upgrade-native-operation','upgrade-owner','upgrade-machine','upgrade-machine',1,'upgrade-assignment',1,'settings/editor',repeat('a',64),'remote','upgrade-cli-session','upgrade-machine','personal',now()+interval '1 minute')`)
	exec(`INSERT INTO paperboat.config_compare_admissions(id,operation_id,account_id,environment_id,machine_id,installation_generation,assignment_id,assignment_version,config_path,conflict_revision,expected_remote_revision,browser_session_id,browser_key_sha256,ticket_hash,workspace_id,expires_at,origin,runtime_host,edge_node_id,edge_process_epoch) VALUES('upgrade-browser-admission','upgrade-browser-operation','upgrade-owner','upgrade-machine','upgrade-machine',1,'upgrade-assignment',1,'settings/editor',repeat('a',64),'remote','upgrade-browser-session',repeat('A',43),decode(repeat('01',32),'hex'),'personal',now()+interval '1 minute','https://dashboard.example.test','machine.runtime.example.test','edge_1','epoch_1')`)
	// Compare at version 61: columns that later migrations add or backfill are not part of this upgrade.
	if err := goose.UpToContext(ctx, store.SQL(), "migrations", 61); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(true); after != before {
		t.Fatalf("upgrade changed durable identity, ownership, publication or generations: before=%s after=%s", before, after)
	}
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	var valid bool
	err = store.SQL().QueryRowContext(ctx, `SELECT
 (SELECT max(version_id)=$1 FROM paperboat.goose_db_version WHERE is_applied)
 AND (SELECT display_name IS NULL FROM paperboat.teams WHERE team_id='upgrade-team')
 AND to_regclass('paperboat.account_network_preferences') IS NULL
 AND to_regclass('paperboat.team_network_preferences') IS NULL
 AND (SELECT configured_capabilities=ARRAY['preview_launch'] AND observed_capabilities=ARRAY['preview_launch'] AND capabilities_desired_version=5 AND capabilities_status='pending' FROM paperboat.user_machines WHERE id='upgrade-machine')
 AND (SELECT owner_team_id IS NULL FROM paperboat.tunnels WHERE id='upgrade-tunnel')
 AND (SELECT owner_team_id IS NULL FROM paperboat.preview_leases WHERE id='upgrade-preview')
 AND paperboat.workspace_tunnel_management_allowed('upgrade-owner',NULL,'upgrade-tunnel')
 AND paperboat.workspace_preview_management_allowed('upgrade-owner',NULL,'upgrade-preview')`, latestMigrationVersion(t)).Scan(&valid)
	if err != nil || !valid {
		t.Fatalf("upgrade retirement/Personal authority invariant: valid=%v error=%v", valid, err)
	}
	var nativeRetained, browserRetained, nativeMetered bool
	err = store.SQL().QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM paperboat.config_compare_admissions WHERE id='upgrade-native-admission'),
 EXISTS(SELECT 1 FROM paperboat.config_compare_admissions WHERE id='upgrade-browser-admission'),
 EXISTS(SELECT 1 FROM paperboat.native_usage_authorities WHERE access_session_id='upgrade-native-admission')`).Scan(&nativeRetained, &browserRetained, &nativeMetered)
	if err != nil || !nativeRetained || browserRetained || !nativeMetered {
		t.Fatalf("comparison admission migration: native=%v browser=%v native usage=%v error=%v", nativeRetained, browserRetained, nativeMetered, err)
	}
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
}
