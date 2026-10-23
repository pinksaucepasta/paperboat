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
	"github.com/pressly/goose/v3"
)

func TestSSHKeysetIdentityMigrationPreservesTrust(t *testing.T) {
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 31); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('user_rekey','rekey','rekey@example.test','active')`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES('machine_rekey','user_rekey','environment_rekey','rekey','linux','amd64','/workspace','online','occupied')`)
	for index, state := range []string{"active", "pending", "superseded"} {
		id := fmt.Sprintf("sshks_rekey_fixture_%016d", index)
		exec(`INSERT INTO paperboat.machine_ssh_host_key_owners VALUES(decode(repeat($1,32),'hex'),'machine_rekey','ssh-ed25519',repeat('k',100)||$1,now())`, fmt.Sprintf("%02x", index+1))
		exec(`INSERT INTO paperboat.machine_ssh_host_key_sets(id,user_machine_id,machine_generation,observation_generation,set_fingerprint,state,reconciliation_version,observed_at,promoted_at) VALUES($1,'machine_rekey',1,$2,decode(repeat($3,32),'hex'),$4,7,now(),CASE WHEN $4='pending' THEN NULL ELSE now() END)`, id, index+1, fmt.Sprintf("%02x", index+1), state)
		exec(`INSERT INTO paperboat.machine_ssh_host_keys VALUES($1,'machine_rekey',decode(repeat($2,32),'hex'),0)`, id, fmt.Sprintf("%02x", index+1))
		exec(`INSERT INTO paperboat.managed_ssh_operations VALUES($1,'user_rekey','host_keys_observe',decode(repeat('ab',32),'hex'),$2,7,now())`, fmt.Sprintf("operation_rekey_fixture_%d", index), id)
	}
	exec(`INSERT INTO paperboat.audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,metadata) VALUES('event_rekey_history','user_rekey','user','managed_ssh.host_key_set_observed','machine_ssh_host_key_set','sshks_rekey_fixture_0000000000000000','{"state":"active"}')`)
	// Preserve every non-identity field, including trust state and timestamps.
	exec(`CREATE TEMP TABLE rekey_expected AS SELECT observation_generation,to_jsonb(s)-'id' AS value FROM paperboat.machine_ssh_host_key_sets s`)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 32); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SQL().QueryContext(ctx, `SELECT id FROM paperboat.machine_ssh_host_key_sets ORDER BY observation_generation`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		parsed, err := uuid.Parse(strings.TrimPrefix(id, "keyset_"))
		if err != nil || id != "keyset_"+parsed.String() || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 {
			t.Fatalf("noncanonical migrated identity %q: %v", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var intact bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT
 (SELECT count(*)=3 AND bool_and(e.value=to_jsonb(s)-'id') FROM paperboat.machine_ssh_host_key_sets s JOIN rekey_expected e USING(observation_generation))
 AND (SELECT count(*)=3 FROM paperboat.machine_ssh_host_keys k JOIN paperboat.machine_ssh_host_key_sets s ON s.id=k.set_id AND s.user_machine_id=k.user_machine_id JOIN paperboat.machine_ssh_host_key_owners o ON o.fingerprint=k.fingerprint AND o.user_machine_id=k.user_machine_id WHERE k.ordinal=0 AND o.public_key=repeat('k',100)||lpad(to_hex(s.observation_generation),2,'0') AND k.fingerprint=s.set_fingerprint)
 AND (SELECT count(*)=3 FROM paperboat.managed_ssh_operations o JOIN paperboat.machine_ssh_host_key_sets s ON s.id=o.resource_id WHERE o.request_hash=decode(repeat('ab',32),'hex') AND o.result_revision=7)
 AND (SELECT resource_id='sshks_rekey_fixture_0000000000000000' AND metadata='{"state":"active"}'::jsonb FROM paperboat.audit_events WHERE id='event_rekey_history')
 AND (SELECT count(*)=3 FROM paperboat.audit_events WHERE event_type='managed_ssh.host_key_set_identity_rekeyed' AND actor_type='system' AND account_id='user_rekey' AND resource_id=metadata->>'new_set_id')`).Scan(&intact); err != nil || !intact {
		t.Fatalf("migration lost trust/history/receipt integrity: intact=%v error=%v", intact, err)
	}
	// A canonical row is stable when the migration body is applied again.
	exec(`CREATE TEMP TABLE rekey_canonical_ids AS SELECT id FROM paperboat.machine_ssh_host_key_sets`)
	body, err := migrationsFS.ReadFile(migrationsDir + "/032_ssh_host_key_set_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*)=3 FROM paperboat.machine_ssh_host_key_sets s JOIN rekey_canonical_ids c USING(id)) AND (SELECT count(*)=3 FROM paperboat.audit_events WHERE event_type='managed_ssh.host_key_set_identity_rekeyed') AND to_regclass('pg_temp.ssh_keyset_identity_rekeys') IS NULL`).Scan(&intact); err != nil || !intact {
		t.Fatalf("canonical reapplication changed identity/audit or retained mapping: %v %v", intact, err)
	}
}
