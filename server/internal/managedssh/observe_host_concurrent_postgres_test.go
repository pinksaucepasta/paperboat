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

// An ObserveHost call waits for a concurrent write to its machine row (a config generation bump) instead of failing
// with 40001 and replaying.
func TestObserveHostWaitsForAMachineWritePostgres(t *testing.T) {
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
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + time.Now().Format("150405.000000000")
	suffix = strings.ReplaceAll(suffix, ".", "")
	user, machine := "obs_user_"+suffix, "obs_machine_"+suffix
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
		set HostKeySet
		err error
	}
	done := make(chan result, 1)
	go func() {
		set, err := service.ObserveHost(ctx, ObserveHostRequest{OperationID: "operation_host_observe_" + suffix, UserID: user, UserMachineID: machine, MachineGeneration: 1, ObservationGeneration: 1, PublicKeys: []string{publicLine(t, "ed25519")}, Now: now})
		done <- result{set, err}
	}()
	for waiting := false; !waiting; {
		select {
		case r := <-done:
			t.Fatalf("the observation finished before the machine write committed: %v", r.err)
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
	if r.err != nil || r.set.State != "active" || len(r.set.Keys) != 1 {
		t.Fatalf("host set %+v, error %v", r.set, r.err)
	}
	after := store.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("ObserveHost retried %d transaction attempts (40001/40P01)", n)
	}
}
