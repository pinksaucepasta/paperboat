package previewtunnelstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A preview dispatch that times out marks its create operation uncertain in phase 'dispatch'. The baseline
// operations_phase_check did not list that phase, so the write failed on top of the 503 (migration 291).
func TestPreviewDispatchUncertainWritesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the preview dispatch PostgreSQL test")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	store, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	account, machine := "usr_dispatch_"+suffix, "machine_dispatch_"+suffix
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`,
		account, "workos_dispatch_"+suffix, "dispatch-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, account); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine); err != nil {
			t.Error(err)
		}
	}()
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := sha256.Sum256([]byte("preview-dispatch:" + suffix))
	created, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
		OperationID: "op_dispatch_" + suffix, LeaseID: "prv_dispatch_" + suffix, AuditEventID: "aud_create_" + suffix,
		AccountID: account, ActorID: account, ActorType: "user", OwnerMachineID: machine, OwnerSessionID: "session_" + suffix, OwnerSessionKind: "foreground",
		TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://" + suffix + ".preview.example.test", LeaseDeadline: now.Add(2 * time.Minute),
		RequestHash: hash[:], IdempotencyKey: "preview-dispatch:" + suffix, CorrelationID: "cor_create_" + suffix,
		RequestID: "req_create_" + suffix, SourceMachineID: machine, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mark := MarkPreviewLeaseDispatchUncertainV1Input{
		AuditEventID: "aud_uncertain_" + suffix, AccountID: account, ActorID: account, ActorType: "user", PreviewID: created.Lease.ID,
		ErrorCode: "timeout", CorrelationID: "cor_uncertain_" + suffix, RequestID: "req_uncertain_" + suffix, SourceMachineID: machine, Now: now.Add(time.Second),
	}
	if err := store.MarkPreviewLeaseDispatchUncertainV1(ctx, mark); err != nil {
		t.Fatalf("mark the create operation uncertain: %v", err)
	}
	var phase, state, outcome string
	if err := database.SQL().QueryRowContext(ctx, `SELECT phase, state, outcome FROM paperboat.operations WHERE id=$1`, created.Operation.ID).Scan(&phase, &state, &outcome); err != nil {
		t.Fatal(err)
	}
	if phase != "dispatch" || state != "uncertain" || outcome != "uncertain" {
		t.Fatalf("operation = phase %q state %q outcome %q, want dispatch/uncertain/uncertain", phase, state, outcome)
	}
}
