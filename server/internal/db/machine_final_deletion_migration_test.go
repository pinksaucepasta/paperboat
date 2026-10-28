package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pressly/goose/v3"
)

func TestMachineFinalDeletionMigrationFrom210(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_MIGRATION_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires empty isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var exists bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("requires empty database: %v", err)
	}
	exec(`CREATE SCHEMA IF NOT EXISTS paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, store.SQL(), migrationsDir, 210); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('final-source','final-source','source@example.test','active'),('final-dest','final-dest','dest@example.test','active')`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES('final-live','final-source','final-live','source-name','linux','amd64','/workspace','offline','released'),('final-removed','final-dest','final-removed','dest-name','linux','amd64','/workspace','offline','released')`)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES('final-live','final-live','final-source'),('final-removed','final-removed','final-dest'),('final-orphan','final-orphan','final-source')`)
	exec(`INSERT INTO paperboat.control_helpers(id,environment_id) VALUES('orphan-helper','final-orphan'),('removed-helper','final-removed')`)
	exec(`INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name) VALUES('orphan-repo','final-source','github','orphan-repo','Orphan config')`)
	exec(`INSERT INTO paperboat.control_config_sync_statuses(environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,machine_updated_at) VALUES('final-orphan','orphan-repo','orphan-assignment','final-orphan',1,'warning','policy',1,'healthy',now())`)
	exec(`INSERT INTO paperboat.control_config_repository_access_operations(operation_id,request_hash,repository_id,assignment_id,environment_id,machine_id,installation_generation,warning_revision) VALUES('orphan-access',decode(repeat('11',32),'hex'),'orphan-repo','orphan-assignment','final-orphan','final-orphan',1,'warning')`)
	exec(`INSERT INTO paperboat.control_config_repository_lease_authority(repository_id,last_fencing_token,lease_id,assignment_id,environment_id,machine_id,installation_generation,operation_id,acquired_at,expires_at) VALUES('orphan-repo',7,'orphan-lease','orphan-assignment','final-orphan','final-orphan',1,'orphan-operation',now(),now()+interval '1 hour')`)
	exec(`UPDATE paperboat.user_machines SET deleted_at=now(),state='deleted' WHERE id='final-removed'`)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES('final-team','final-source',1)`)
	exec(`INSERT INTO paperboat.team_inbox_requests(request_id,operation_id,team_id,sender_account,recipient_account,source_machine_id,destination_machine_id,batch_id,manifest_digest,manifest,status,decision_kind,policy_generation,expires_at) VALUES('final-request','final-operation','final-team','final-source','final-dest','final-live','final-removed','final-batch',repeat('a',64),'[{"basename":"fixture","size":1}]','completed','manual',1,now()+interval '1 hour')`)
	exec(`INSERT INTO paperboat.team_inbox_receipt_outbox(request_id,recipient_account) VALUES('final-request','final-dest')`)
	exec(`INSERT INTO paperboat.user_machine_bandwidth_periods(user_machine_id,period_start,period_end,included_bytes,consumed_included_bytes) VALUES('final-live',now(),now()+interval '1 month',1000,123)`)
	if err := goose.UpToContext(ctx, store.SQL(), migrationsDir, 211); err != nil {
		t.Fatal(err)
	}
	var cleaned bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM paperboat.control_environments WHERE id IN ('final-orphan','final-removed')) AND NOT EXISTS(SELECT 1 FROM paperboat.control_helpers WHERE id IN ('orphan-helper','removed-helper')) AND EXISTS(SELECT 1 FROM paperboat.control_environments WHERE id='final-live' AND machine_id='final-live')`).Scan(&cleaned); err != nil || !cleaned {
		t.Fatalf("orphan cleanup=%v: %v", cleaned, err)
	}
	var operationalCleanup bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM paperboat.control_config_sync_statuses WHERE environment_id='final-orphan') AND NOT EXISTS(SELECT 1 FROM paperboat.control_config_repository_access_operations WHERE operation_id='orphan-access') AND EXISTS(SELECT 1 FROM paperboat.control_config_repository_lease_authority WHERE repository_id='orphan-repo' AND lease_id IS NULL AND last_fencing_token=7)`).Scan(&operationalCleanup); err != nil || !operationalCleanup {
		t.Fatalf("orphan config cleanup and fencing counter=%v: %v", operationalCleanup, err)
	}
	exec(`DELETE FROM paperboat.user_machines WHERE id IN ('final-live','final-removed')`)
	var retained bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.team_inbox_requests WHERE request_id='final-request' AND source_machine_id='final-live' AND destination_machine_id='final-removed' AND source_machine_name='source-name' AND destination_machine_name='dest-name') AND EXISTS(SELECT 1 FROM paperboat.team_inbox_receipt_outbox WHERE request_id='final-request' AND status='pending') AND EXISTS(SELECT 1 FROM paperboat.user_machine_bandwidth_periods WHERE user_machine_id='final-live' AND user_id='final-source' AND machine_name='source-name' AND consumed_included_bytes=123)`).Scan(&retained); err != nil || !retained {
		t.Fatalf("backfilled history lost=%v: %v", !retained, err)
	}
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES('final-standalone','final-standalone','final-source')`)
	var standalone bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT machine_id IS NULL FROM paperboat.control_environments WHERE id='final-standalone'`).Scan(&standalone); err != nil || !standalone {
		t.Fatalf("standalone origin=%v: %v", standalone, err)
	}
}
