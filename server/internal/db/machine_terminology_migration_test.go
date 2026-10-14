package db

import (
	"context"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pressly/goose/v3"
	"os"
	"testing"
	"time"
)

func TestMachineTerminologyMigrationFrom28(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("requires empty isolated database")
	}
	exec := func(q string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA IF NOT EXISTS paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 28); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('machine_terminology_owner','machine-terminology','machine-terminology@example.test','active')`)
	exec(`INSERT INTO paperboat.account_network_preferences(account_id,revision,device_suffix,device_loopback_cidr) VALUES('machine_terminology_owner',7,'custom-paperboat','127.101.0.0/16')`)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES('machine_terminology_environment','machine_terminology_existing','machine_terminology_owner')`)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,installation_generation,worker_generation,device_service_policy_generation,public_identity_key) VALUES('machine_terminology_existing','machine_terminology_owner','machine_terminology_environment','terminology','linux','amd64','/workspace','online','occupied',7,4,9,'AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE')`)
	exec(`INSERT INTO paperboat.control_config_assignments(id,environment_id,machine_id,path_rules) VALUES('machine_terminology_config','machine_terminology_environment','machine_terminology_existing','[{"id":"settings","source":"device","local_path":"/home/device/settings.json"},{"id":"team","source":"team"}]')`)
	check := func(canonical bool) {
		t.Helper()
		prefix := "device"
		if canonical {
			prefix = "machine"
		}
		var suffix, cidr, key, source, path string
		var revision, generation, worker, policy int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT `+prefix+`_suffix,`+prefix+`_loopback_cidr,revision FROM paperboat.account_network_preferences WHERE account_id='machine_terminology_owner'`).Scan(&suffix, &cidr, &revision); err != nil {
			t.Fatal(err)
		}
		if suffix != "custom-paperboat" || cidr != "127.101.0.0/16" || revision != 7 {
			t.Fatal("network preference data changed")
		}
		if err := store.SQL().QueryRowContext(ctx, `SELECT public_identity_key,installation_generation,worker_generation,`+prefix+`_service_policy_generation FROM paperboat.user_machines WHERE id='machine_terminology_existing'`).Scan(&key, &generation, &worker, &policy); err != nil {
			t.Fatal(err)
		}
		if key != "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" || generation != 7 || worker != 4 || policy != 9 {
			t.Fatal("installed identity or generation changed")
		}
		if err := store.SQL().QueryRowContext(ctx, `SELECT path_rules->0->>'source',path_rules->0->>'local_path' FROM paperboat.control_config_assignments WHERE id='machine_terminology_config'`).Scan(&source, &path); err != nil {
			t.Fatal(err)
		}
		if source != prefix || path != "/home/device/settings.json" {
			t.Fatal("path rule source or user path changed")
		}
		var allowed bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.`+prefix+`_service_authorities('machine_terminology_owner','machine_terminology_existing',5432))`).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Fatal("owner automatic-service authority lost during rename")
		}
		if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.`+prefix+`_service_authorities('unknown_actor','machine_terminology_existing',5432))`).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatal("unknown actor gained machine service authority")
		}
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 29); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err = goose.DownToContext(ctx, store.SQL(), migrationsDir, 28); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 29); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err = Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"paperboat.account_network_preferences", "paperboat.team_network_preferences"} {
		var exists bool
		if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("retired table %s still exists after migration", table)
		}
	}
}
