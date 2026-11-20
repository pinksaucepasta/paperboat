package previewtunnelstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Marking a preview ready waits for a concurrent write to its lease row (its generation CAS re-checks the new row
// version) instead of failing with 40001 and replaying.
func TestMarkPreviewLeaseReadyWaitsForALeaseWritePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the preview lease PostgreSQL test")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	store, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	account, machine := "usr_prvready_"+suffix, "machine_prvready_"+suffix
	t.Cleanup(func() {
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
		database.Close()
	})
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, account, "workos_"+account, account+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities,updated_at) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[],now()-interval '1 hour')`, machine, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := sha256Bytes("preview-create:" + suffix)
	created, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
		OperationID: "op_" + suffix, LeaseID: "prv_" + suffix, AuditEventID: "aud_" + suffix, AccountID: account, ActorID: account, ActorType: "user",
		OwnerMachineID: machine, OwnerSessionID: "session_" + suffix, OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000",
		AccessMode: "public", Endpoint: "https://" + suffix + ".preview.example.test", LeaseDeadline: now.Add(2 * time.Minute), RequestHash: hash,
		IdempotencyKey: "preview-create:" + suffix, CorrelationID: "cor_" + suffix, RequestID: "req_" + suffix, SourceMachineID: machine, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	writer, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE paperboat.preview_leases SET lease_deadline=lease_deadline WHERE id=$1`, created.Lease.ID); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	before := database.TransactionStats()
	type result struct {
		generation int64
		err        error
	}
	done := make(chan result, 1)
	go func() {
		ready, err := store.MarkPreviewLeaseReadyV1(ctx, MarkPreviewLeaseReadyV1Input{
			AuditEventID: "aud_ready_" + suffix, AccountID: account, ActorID: account, ActorType: "user", PreviewID: created.Lease.ID, ExpectedGeneration: 1,
			AllocationState: "ready", EdgeState: "ready", OriginState: "ready", CorrelationID: "cor_ready_" + suffix, RequestID: "req_ready_" + suffix,
			SourceMachineID: machine, Now: now.Add(time.Second),
		})
		done <- result{ready.Generation, err}
	}()
	for waiting := false; !waiting; {
		select {
		case r := <-done:
			t.Fatalf("mark ready finished before the lease write committed: %v", r.err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := database.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.generation != 2 {
		t.Fatalf("ready generation %d, error %v", r.generation, r.err)
	}
	after := database.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("mark ready retried %d transaction attempts (40001/40P01)", n)
	}
}
