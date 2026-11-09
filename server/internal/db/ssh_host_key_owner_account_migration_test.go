package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pressly/goose/v3"
)

// Per-account host-key ownership. Expand (238-240): owners get their account (backfill and a trigger for the
// running server's inserts) and nothing the running server uses is dropped. Contract (243-245): account_id is
// required, the global key is dropped, cross-account conflicts are resolved and same-account ones stay.
func TestSSHHostKeyOwnerAccountMigration(t *testing.T) {
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
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 237); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"hkacct_a", "hkacct_b"} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	}
	for _, m := range [][2]string{{"hkacct_a1", "hkacct_a"}, {"hkacct_a2", "hkacct_a"}, {"hkacct_b1", "hkacct_b"}} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,'env_'||$1,'linux','amd64','/workspace','online','occupied')`, m[0], m[1])
	}
	exec(`INSERT INTO paperboat.machine_ssh_host_key_owners VALUES(decode(repeat('01',32),'hex'),'hkacct_a1','ssh-ed25519',repeat('k',100),now())`)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 240); err != nil {
		t.Fatal(err)
	}
	// An insert by the running server (no account_id) is filled by the trigger, and the global key it relies on stays.
	exec(`INSERT INTO paperboat.machine_ssh_host_key_owners(fingerprint,user_machine_id,algorithm,public_key,first_observed_at) VALUES(decode(repeat('02',32),'hex'),'hkacct_b1','ssh-ed25519',repeat('j',100),now())`)
	var ok bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT
 (SELECT account_id FROM paperboat.machine_ssh_host_key_owners WHERE user_machine_id='hkacct_a1')='hkacct_a'
 AND (SELECT account_id FROM paperboat.machine_ssh_host_key_owners WHERE user_machine_id='hkacct_b1')='hkacct_b'
 AND (SELECT count(*)=0 FROM paperboat.machine_ssh_host_key_owners WHERE account_id IS NULL)`).Scan(&ok); err != nil || !ok {
		t.Fatalf("account_id: ok=%v error=%v", ok, err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.machine_ssh_host_key_owners(fingerprint,user_machine_id,algorithm,public_key,first_observed_at) VALUES(decode(repeat('01',32),'hex'),'hkacct_b1','ssh-ed25519',repeat('k',100),now())`); err == nil {
		t.Fatal("the global host-key owner key was dropped while the running server still uses it")
	}
	// Contract: cross-account conflicts are resolved (same-account ones stay) and one fingerprint is owned once per account.
	exec(`INSERT INTO paperboat.machine_ssh_host_key_conflicts VALUES('hkacct_b1','hkacct_a1',now()),('hkacct_a2','hkacct_a1',now())`)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 245); err != nil {
		t.Fatal(err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT (SELECT array_agg(user_machine_id)=ARRAY['hkacct_a2'] FROM paperboat.machine_ssh_host_key_conflicts)
 AND (SELECT is_nullable='NO' FROM information_schema.columns WHERE table_schema='paperboat' AND table_name='machine_ssh_host_key_owners' AND column_name='account_id')`).Scan(&ok); err != nil || !ok {
		t.Fatalf("contract: ok=%v error=%v", ok, err)
	}
	owner := `INSERT INTO paperboat.machine_ssh_host_key_owners(fingerprint,user_machine_id,account_id,algorithm,public_key,first_observed_at) VALUES(decode(repeat('01',32),'hex'),$1,$2,'ssh-ed25519',repeat('k',100),now())`
	exec(owner, "hkacct_b1", "hkacct_b")
	if _, err = store.SQL().ExecContext(ctx, owner, "hkacct_a2", "hkacct_a"); err == nil {
		t.Fatal("one account owns the same host key twice")
	}
}
