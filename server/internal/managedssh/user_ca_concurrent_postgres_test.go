package managedssh

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A host's CA fetch waits for a concurrent write to its machine row (a config generation bump) instead of failing
// with 40001 and replaying.
func TestUserCAFetchWaitsForAMachineWritePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + time.Now().Format("150405.000000000")
	suffix = strings.ReplaceAll(suffix, ".", "")
	user, machine := "ca_user_"+suffix, "ca_machine_"+suffix
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
		store.Close()
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, user, "env_"+machine); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, testCertificates)

	writer, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE paperboat.user_machines SET updated_at=now() WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	before := store.TransactionStats()
	type result struct {
		set TrustedUserCASet
		err error
	}
	done := make(chan result, 1)
	go func() {
		set, err := service.TrustedUserCAs(ctx, UserCAsRequest{ActorUserID: user, UserMachineID: machine, MachineGeneration: 1, Now: now})
		done <- result{set, err}
	}()
	for waiting := false; !waiting; {
		select {
		case r := <-done:
			t.Fatalf("the fetch finished before the machine write committed: %v", r.err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || len(r.set.Keys) != 1 {
		t.Fatalf("CA set %+v, error %v", r.set, r.err)
	}
	after := store.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("the CA fetch retried %d transaction attempts (40001/40P01)", n)
	}
}

// The authority lock must not block the foreign-key checks of rows that name its owner: an audit or preview write
// holds KEY SHARE on the account row and then updates the machine row the fetch holds (a deadlock in prod).
func TestUserCAFetchDoesNotBlockOnAKeyShareOfTheOwnerPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ReplaceAll(strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))+time.Now().Format("150405.000000000"), ".", "")
	user, machine := "cak_user_"+suffix, "cak_machine_"+suffix
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
		store.Close()
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, user, "env_"+machine); err != nil {
		t.Fatal(err)
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository, testCertificates)
	holder, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	var locked string
	if err := holder.QueryRow(ctx, `SELECT id FROM paperboat.users WHERE id=$1 FOR KEY SHARE`, user).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	fetch, cancelFetch := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFetch()
	if _, err := service.TrustedUserCAs(fetch, UserCAsRequest{ActorUserID: user, UserMachineID: machine, MachineGeneration: 1, Now: now}); err != nil {
		t.Fatalf("the fetch waited for a KEY SHARE holder of its owner row: %v", err)
	}
}
