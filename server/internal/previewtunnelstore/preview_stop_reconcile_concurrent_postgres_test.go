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

// Stopping a lease waits for a concurrent write to its lease row (the
// owner/session/generation CAS in AdvancePreviewLeaseLifecycleV1 re-checks the
// new row version) instead of failing with 40001 and replaying.
func TestStopPreviewLeaseWaitsForALeaseWritePostgres(t *testing.T) {
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
	account, machine := "usr_prvstop_"+suffix, "machine_prvstop_"+suffix
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, "prv_"+suffix)
		database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id=$1`, account)
		database.Close()
	})
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, account, "workos_"+account, account+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities,updated_at) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[],now()-interval '1 hour')`, machine, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
		OperationID: "op_" + suffix, LeaseID: "prv_" + suffix, AuditEventID: "aud_" + suffix, AccountID: account, ActorID: account, ActorType: "user",
		OwnerMachineID: machine, OwnerSessionID: "session_" + suffix, OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000",
		AccessMode: "public", Endpoint: "https://" + suffix + ".preview.example.test", LeaseDeadline: now.Add(2 * time.Minute), RequestHash: sha256Bytes("preview-create:" + suffix),
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
		lease PreviewLeaseRecord
		err   error
	}
	done := make(chan result, 1)
	go func() {
		stopped, err := store.StopPreviewLeaseV1(ctx, StopPreviewLeaseV1Input{
			OperationID: "op_stop_" + suffix, AuditEventID: "aud_stop_" + suffix, AccountID: account, ActorID: account, ActorType: "user",
			PreviewID: created.Lease.ID, OwnerMachineID: machine, OwnerSessionID: "session_" + suffix, ExpectedGeneration: 1,
			RequestHash: sha256Bytes("preview-stop:" + suffix), IdempotencyKey: "preview-stop:" + suffix,
			CorrelationID: "cor_stop_" + suffix, RequestID: "req_stop_" + suffix, SourceMachineID: machine, Now: now.Add(time.Second),
		})
		done <- result{stopped.Lease, err}
	}()
	for waiting := false; !waiting; {
		select {
		case r := <-done:
			t.Fatalf("stop finished before the lease write committed: %v", r.err)
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
	r := <-done
	if r.err != nil {
		t.Fatalf("stop error: %v", r.err)
	}
	if r.lease.TerminalState != "stopped" || r.lease.Generation != 2 {
		t.Fatalf("stopped lease terminal=%q generation=%d", r.lease.TerminalState, r.lease.Generation)
	}
	after := database.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("stop retried %d transaction attempts (40001/40P01)", n)
	}
}

// Reconciling leases leaves an expired lease that another transaction is writing to the next pass (the batch queries
// claim FOR UPDATE SKIP LOCKED) instead of failing with 40001 and replaying; the next pass expires it.
func TestReconcilePreviewLeasesSkipsALeaseBeingWrittenPostgres(t *testing.T) {
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
	account, machine := "usr_prvrecon_"+suffix, "machine_prvrecon_"+suffix
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, "prv_"+suffix)
		database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id=$1`, account)
		database.Close()
	})
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, account, "workos_"+account, account+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// The owner has been online for long: a lapsed lease_deadline ends the
	// lease once the owner was online for the grace and still did not renew.
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities,updated_at) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[],now()-interval '1 hour')`, machine, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
		OperationID: "op_" + suffix, LeaseID: "prv_" + suffix, AuditEventID: "aud_" + suffix, AccountID: account, ActorID: account, ActorType: "user",
		OwnerMachineID: machine, OwnerSessionID: "session_" + suffix, OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000",
		AccessMode: "public", Endpoint: "https://" + suffix + ".preview.example.test", LeaseDeadline: now.Add(2 * time.Minute), RequestHash: sha256Bytes("preview-create:" + suffix),
		IdempotencyKey: "preview-create:" + suffix, CorrelationID: "cor_" + suffix, RequestID: "req_" + suffix, SourceMachineID: machine, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Lapse the deadline (it must stay after created_at: preview_leases_check)
	// so the lease is expired by deadline at reconcile time.
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET lease_deadline=$1 WHERE id=$2`, now.Add(10*time.Second), created.Lease.ID); err != nil {
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
	reconcile := func() (int, string) {
		t.Helper()
		reconciled, err := store.ReconcilePreviewLeasesV1(ctx, ReconcilePreviewLeasesV1Input{
			ActorID: "test", ActorType: "system", CorrelationID: "cor_recon_" + suffix, RequestID: "req_recon_" + suffix,
			Now: now.Add(time.Minute), OwnerGrace: 5 * time.Minute, BackgroundOwnerGrace: 5 * time.Minute, Limit: 10,
		})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}
		for _, lease := range reconciled.Expired {
			if lease.ID == created.Lease.ID {
				return 1, lease.TerminalState
			}
		}
		return 0, ""
	}
	if n, _ := reconcile(); n != 0 {
		t.Fatalf("a pass expired a lease another transaction was writing")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, state := reconcile(); n != 1 || state != "expired" {
		t.Fatalf("next pass expired=%d state=%q, want 1 expired", n, state)
	}
	after := database.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("reconcile retried %d transaction attempts (40001/40P01)", n)
	}
}
