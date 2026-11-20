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

func TestCreatePreviewLeaseWaitsForATopicWritePostgres(t *testing.T) {
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
	account, machine := "usr_prvcreate_"+suffix, "machine_prvcreate_"+suffix
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
	input := CreatePreviewLeaseV1Input{
		OperationID: "op_" + suffix, LeaseID: "prv_" + suffix, AuditEventID: "aud_" + suffix, AccountID: account, ActorID: account, ActorType: "user",
		OwnerMachineID: machine, OwnerSessionID: "session_" + suffix, OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000",
		AccessMode: "public", Endpoint: "https://" + suffix + ".preview.example.test", LeaseDeadline: now.Add(2 * time.Minute), RequestHash: hash,
		IdempotencyKey: "preview-create:" + suffix, CorrelationID: "cor_" + suffix, RequestID: "req_" + suffix, SourceMachineID: machine, Now: now,
	}

	writer, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `SELECT paperboat.publish_machine_topic(ARRAY[$1::text], 'previews')`, machine); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	before := database.TransactionStats()
	done := make(chan error, 1)
	go func() {
		created, err := store.CreatePreviewLeaseV1(ctx, input)
		if err == nil && created.Lease.ID != input.LeaseID {
			err = fmt.Errorf("created lease %q, want %q", created.Lease.ID, input.LeaseID)
		}
		done <- err
	}()
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("create finished before the topic write committed: %v", err)
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
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := database.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("create retried %d transaction attempts (40001/40P01)", n)
	}
}
