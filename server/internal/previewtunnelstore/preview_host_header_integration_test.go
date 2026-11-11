package previewtunnelstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// pb preview --host-header: the Host the origin receives is stored with the lease and read back by Get and
// List; the database refuses one on a target with no HTTP request to rewrite.
func TestPreviewHostHeaderOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the preview host header PostgreSQL test")
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
	owner, machine := "usr_hh_"+suffix, "machine_hh_"+suffix
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $2, $3, 'active')`, owner, "workos_"+owner, owner+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	input := func(name, scheme, address string, host sql.NullString) CreatePreviewLeaseV1Input {
		return CreatePreviewLeaseV1Input{
			OperationID: "op_" + name + suffix, LeaseID: "prv_" + name + suffix, AuditEventID: "aud_" + name + suffix,
			AccountID: owner, ActorID: owner, ActorType: "user", OwnerMachineID: machine, OwnerSessionID: "sess_" + name + suffix, OwnerSessionKind: "foreground",
			TargetScheme: scheme, TargetAddress: address, HostHeader: host, AccessMode: "public", Endpoint: "https://" + name + suffix + ".preview.example.test",
			LeaseDeadline: now.Add(time.Hour), RequestHash: sha256Bytes(name + suffix), IdempotencyKey: name + suffix,
			CorrelationID: "cor_" + name + suffix, RequestID: "req_" + name + suffix, Now: now,
		}
	}
	created, err := store.CreatePreviewLeaseV1(ctx, input("hh", "http", "127.0.0.1:3000", sql.NullString{String: "app.example.test", Valid: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := created.Lease.HostHeader; !got.Valid || got.String != "app.example.test" {
		t.Fatalf("created host header = %#v", got)
	}
	read, err := store.GetPreviewLeaseV1(ctx, owner, created.Lease.ID)
	if err != nil || !read.HostHeader.Valid || read.HostHeader.String != "app.example.test" {
		t.Fatalf("read host header = %#v, %v", read.HostHeader, err)
	}
	plain, err := store.CreatePreviewLeaseV1(ctx, input("plain", "http", "127.0.0.1:3001", sql.NullString{}))
	if err != nil || plain.Lease.HostHeader.Valid {
		t.Fatalf("a preview with no host header: %#v, %v", plain.Lease.HostHeader, err)
	}
	if _, err := store.CreatePreviewLeaseV1(ctx, input("bad", "unix", "/tmp/app.sock", sql.NullString{String: "app.example.test", Valid: true})); err == nil {
		t.Fatal("a unix target took a host header")
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET target_scheme='unix', target_address='/tmp/x.sock' WHERE id=$1`, created.Lease.ID); err == nil {
		t.Fatal("the database let a host header ride on a unix target")
	}
}
