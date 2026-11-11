package previewtunnelstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// A shared Inbox file is a preview whose target is the file id on its machine: the database takes the
// "file" scheme, only the machine's owner may publish it, it counts toward the plan's concurrent
// previews, and it ends at its deadline like any preview.
func TestPreviewFileTargetOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the preview file target PostgreSQL test")
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
	limits := config.Default().Billing.Limits
	limits.Publications.Free = 1
	planlimits.Configure(limits, "")
	t.Cleanup(func() { planlimits.Configure(config.Default().Billing.Limits, "") })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, stranger, machine := "usr_file_"+suffix, "usr_file_other_"+suffix, "machine_file_"+suffix
	for _, user := range []string{owner, stranger} {
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
	})

	if _, err := store.VerifyPreviewLeaseOwnerV1(ctx, owner, machine, "public"); err != nil {
		t.Fatalf("owner may publish the machine's file: %v", err)
	}
	if _, err := store.VerifyPreviewLeaseOwnerV1(ctx, stranger, machine, "public"); err == nil {
		t.Fatal("another account must not publish this machine's Inbox file")
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	input := func(name, address string, deadline time.Time) CreatePreviewLeaseV1Input {
		return CreatePreviewLeaseV1Input{
			OperationID: "op_" + name + suffix, LeaseID: "prv_" + name + suffix, AuditEventID: "aud_" + name + suffix,
			AccountID: owner, ActorID: owner, ActorType: "user", OwnerMachineID: machine, OwnerSessionID: "console_" + name + suffix, OwnerSessionKind: "foreground",
			TargetScheme: "file", TargetAddress: address, AccessMode: "public", Endpoint: "https://" + name + suffix + ".preview.example.test",
			LeaseDeadline: deadline, RequestHash: sha256Bytes(name + suffix), IdempotencyKey: name + suffix,
			CorrelationID: "cor_" + name + suffix, RequestID: "req_" + name + suffix, Now: now,
		}
	}
	if _, err := store.CreatePreviewLeaseV1(ctx, input("badfile", "/etc/passwd", now.Add(time.Hour))); err == nil {
		t.Fatal("a file target must be an Inbox file id, never a path")
	}
	created, err := store.CreatePreviewLeaseV1(ctx, input("file", "0123456789abcdef", now.Add(2*time.Second)))
	if err != nil {
		t.Fatalf("create file preview: %v", err)
	}
	if created.Lease.TargetScheme != "file" || created.Lease.TargetAddress != "0123456789abcdef" {
		t.Fatalf("lease target = %s %s", created.Lease.TargetScheme, created.Lease.TargetAddress)
	}
	var limit *planlimits.LimitError
	if _, err := store.CreatePreviewLeaseV1(ctx, input("second", "fedcba9876543210", now.Add(time.Hour))); !errors.As(err, &limit) || limit.Kind != planlimits.Publications {
		t.Fatalf("a second share past the free plan's one preview: %v", err)
	}

	reconciled, err := store.ReconcilePreviewLeasesV1(ctx, ReconcilePreviewLeasesV1Input{
		ActorID: owner, ActorType: "system", CorrelationID: "cor_reconcile_" + suffix, RequestID: "req_reconcile_" + suffix,
		Now: now.Add(10 * time.Second), OwnerGrace: time.Hour, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired := false
	for _, row := range reconciled.Expired {
		expired = expired || row.ID == created.Lease.ID
	}
	if !expired {
		t.Fatal("a shared file must end at its deadline")
	}
	// Its place is free again once it ended.
	if _, err := store.CreatePreviewLeaseV1(ctx, input("third", "fedcba9876543210", now.Add(time.Hour))); err != nil {
		t.Fatalf("share after the first ended: %v", err)
	}
}
