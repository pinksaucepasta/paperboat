package db

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pressly/goose/v3"
	"os"
	"testing"
	"time"
)

func TestResourceIdentityMigrationFrom27(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("requires empty isolated database")
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA IF NOT EXISTS paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 27); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('usr_id_migration','id-migration','id-migration@example.test','active')`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES('mch_id_migration','usr_id_migration','env_id_migration','id-migration','linux','amd64','/workspace','online','occupied')`)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 28); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,expires_at) VALUES($4,$1,'mch_id_migration','usr_id_migration','env_id_migration','cls_id_migration','https://invalid.test',$2,$3,now()+interval '1 hour')`
	for id, kind := range map[string]string{"session_123e4567-e89b-42d3-a456-426614174000": "control_plane_signed", "session_53cbb3f7-ec2a-4ae8-9cd2-06d7c39cedd2": "external_issuer"} {
		exec(insert, id, "token_2eed839c-08e1-41c4-870b-2f96a5631f37", nil, kind)
		var got string
		if err = store.SQL().QueryRowContext(ctx, `SELECT credential_kind FROM paperboat.user_machine_access_sessions WHERE id=$1`, id).Scan(&got); err != nil || got != kind {
			t.Fatalf("kind=%q want=%q error=%v", got, kind, err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,role) VALUES('operator_123e4567-e89b-42d3-a456-426614174000','operator-id-migration@example.test','fixture-not-used-for-login','support')`)
	exec(`INSERT INTO paperboat.audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata) VALUES('event_id_migration','operator_123e4567-e89b-42d3-a456-426614174000','admin','support.operator_lookup','support_reference','migration-probe','{}')`)
	exec(`CREATE TABLE paperboat.id_migration_actor_probe(actor text)`)
	defer store.SQL().ExecContext(context.Background(), `DROP TABLE paperboat.id_migration_actor_probe`)
	exec(`CREATE TRIGGER named_actor_probe BEFORE INSERT ON paperboat.id_migration_actor_probe FOR EACH ROW EXECUTE FUNCTION paperboat.check_named_operator_actor('actor')`)
	exec(`INSERT INTO paperboat.id_migration_actor_probe(actor) VALUES('operator_123e4567-e89b-42d3-a456-426614174000'),('usr_id_migration'),(NULL)`)
	for _, query := range []string{
		`INSERT INTO paperboat.audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata) VALUES('event_unknown_actor','operator_missing','admin','support.operator_lookup','support_reference','migration-probe','{}')`,
		`INSERT INTO paperboat.id_migration_actor_probe(actor) VALUES('operator_missing')`,
	} {
		_, err := store.SQL().ExecContext(ctx, query)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23503" {
			t.Fatalf("unknown actor should fail foreign-key check: %v", err)
		}
	}
	exec(insert, "session_2eed839c-08e1-41c4-870b-2f96a5631f37", "token_123e4567-e89b-42d3-a456-426614174000", nil, "control_plane_signed")
	exec(`UPDATE paperboat.user_machine_access_sessions SET credential_kind='control_plane_signed' WHERE id='session_2eed839c-08e1-41c4-870b-2f96a5631f37'`)
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET credential_kind='guessed' WHERE id='session_2eed839c-08e1-41c4-870b-2f96a5631f37'`); err == nil {
		t.Fatal("unknown credential kind accepted")
	}
	for _, constraint := range []string{"environment_authorities_operation_id_check", "diagnostic_upload_intents_id_check", "support_reports_support_reference_check"} {
		var expression string
		if err = store.SQL().QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace='paperboat'::regnamespace AND conname=$1`, constraint).Scan(&expression); err != nil {
			t.Fatal(err)
		}
		if expression == "" {
			t.Fatalf("missing constraint %s", constraint)
		}
	}
}
