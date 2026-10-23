package db

import (
	"context"
	"encoding/base64"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pressly/goose/v3"
)

// This test requires its own empty database because it exercises the actual
// applied version 26 -> 27 boundary rather than recreating retired columns.
func TestUnifiedMachineMigrationFrom26(t *testing.T) {
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
	if err := store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.user_machines') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("migration fixture requires an empty isolated database")
	}
	if _, err := store.SQL().ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS paperboat`); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, store.SQL(), migrationsDir, 26); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('usr_unified_migration','unified-migration','unified-migration@example.test','active')`)
	fixtures := []struct {
		id       string
		caps     []string
		revision int64
	}{
		{"default", []string{"file_receive", "preview_launch"}, 1},
		{"explicit", []string{"file_receive", "peer_relay"}, 7},
		{"revision-one-selection", []string{"file_receive"}, 1},
	}
	for index, f := range fixtures {
		keyBytes := make([]byte, 32)
		keyBytes[0] = byte(index + 1)
		key := base64.RawURLEncoding.EncodeToString(keyBytes)
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,setup_mode,setup_roles,configured_capabilities,capabilities_desired_version,installation_generation,public_identity_key) VALUES($1,'usr_unified_migration',$2,$3,'linux','amd64','/workspace','online','released','client',ARRAY['interactive'],$4,$5,8,$6)`, "mch_unified_"+f.id, "env_unified_"+f.id, "unified-"+f.id, f.caps, f.revision, key)
		if f.id == "explicit" {
			exec(`UPDATE paperboat.user_machines SET observed_capabilities=ARRAY['peer_relay'] WHERE id=$1`, "mch_unified_"+f.id)
		}
	}
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES('env_unified_default','mch_unified_default','usr_unified_migration','active')`)
	exec(`INSERT INTO paperboat.control_helpers(id,environment_id,state,public_key,key_thumbprint,generation) VALUES('helper_unified_active','env_unified_default','active',decode(repeat('01',32),'hex'),'unified-active',4),('helper_unified_pending','env_unified_default','pending',NULL,NULL,1)`)
	exec(`INSERT INTO paperboat.control_helper_enrollments(id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES('grant_unified_active','env_unified_default','helper_unified_active',decode('01','hex'),'unified-active',decode('01','hex'),decode('01','hex'),'consumed',now()+interval '1 hour'),('grant_unified_pending','env_unified_default','helper_unified_pending',decode('02','hex'),'helper-enrollment:usr_unified_migration:machine-enrollment:pairing_unified_pending',decode('02','hex'),decode('02','hex'),'pending',now()+interval '1 hour')`)
	exec(`INSERT INTO paperboat.user_machine_pairings(id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,runtime_versions,public_identity_key,state,approved_by_user_id,user_machine_id,installation_config_ciphertext,expires_at) VALUES('pairing_unified_pending',decode('03','hex'),'UNIFIED123','unified-default','linux','amd64','/workspace','{}','AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA','approved','usr_unified_migration','mch_unified_default',decode('03','hex'),now()+interval '1 hour')`)
	// 027 drains the approved pairing and its pending grant. 037 then requires the
	// pairing table to be empty (it adds NOT NULL columns without defaults and drops
	// the enrollment table), so the drained pairing row is removed before the cutover.
	if err := goose.UpToContext(ctx, store.SQL(), migrationsDir, 27); err != nil {
		t.Fatal(err)
	}
	var pendingState, pairingState string
	var materialGone bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.control_helper_enrollments WHERE id='grant_unified_pending'`).Scan(&pendingState); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,installation_config_ciphertext IS NULL FROM paperboat.user_machine_pairings WHERE id='pairing_unified_pending'`).Scan(&pairingState, &materialGone); err != nil {
		t.Fatal(err)
	}
	if pendingState != "revoked" || pairingState != "expired" || !materialGone {
		t.Fatalf("drain before cutover: pending=%s pairing=%s cleared=%v", pendingState, pairingState, materialGone)
	}
	exec(`DELETE FROM paperboat.user_machine_pairings WHERE id='pairing_unified_pending'`)
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	for index, f := range fixtures {
		keyBytes := make([]byte, 32)
		keyBytes[0] = byte(index + 1)
		wantKey := base64.RawURLEncoding.EncodeToString(keyBytes)
		var caps, observed []string
		var revision, generation int64
		var seat, key string
		if err := store.pool.QueryRow(ctx, `SELECT configured_capabilities,observed_capabilities,capabilities_desired_version,installation_generation,seat_state,public_identity_key FROM paperboat.user_machines WHERE id=$1`, "mch_unified_"+f.id).Scan(&caps, &observed, &revision, &generation, &seat, &key); err != nil {
			t.Fatal(err)
		}
		if generation != 8 || seat != "occupied" || key != wantKey {
			t.Fatalf("identity/seat changed for %s: generation=%d seat=%s key=%s", f.id, generation, seat, key)
		}
		if f.id == "explicit" {
			if revision != f.revision+1 || slices.Contains(caps, "peer_relay") || slices.Contains(observed, "peer_relay") || !slices.Contains(caps, "file_receive") {
				t.Fatalf("retired peer relay selection configured=%v observed=%v revision=%d", caps, observed, revision)
			}
			continue
		}
		if f.id == "default" {
			if revision != 2 || !slices.Contains(caps, "terminal_host") || !slices.Contains(caps, "ssh_host") || !slices.Contains(caps, "environment_injection") || slices.Contains(caps, "peer_relay") {
				t.Fatalf("default expansion: %v revision=%d", caps, revision)
			}
		} else if revision != f.revision || !reflect.DeepEqual(caps, f.caps) {
			t.Fatalf("explicit selection changed: %s %v revision=%d", f.id, caps, revision)
		}
	}
	var activeState string
	var generation int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,generation FROM paperboat.control_helpers WHERE id='helper_unified_active'`).Scan(&activeState, &generation); err != nil {
		t.Fatal(err)
	}
	if activeState != "active" || generation != 4 {
		t.Fatalf("cutover authority: active=%s generation=%d", activeState, generation)
	}
}
