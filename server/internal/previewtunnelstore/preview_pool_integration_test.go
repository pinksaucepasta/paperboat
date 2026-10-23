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
)

func TestPreviewLeaseScopedPoolAdmissionOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
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
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	store, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	account, other, machine := "usr_pool_"+suffix, "usr_other_pool_"+suffix, "machine_pool_"+suffix
	exec := func(t *testing.T, query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{account, other} {
		exec(t, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, user, user+"@example.test")
	}
	exec(t, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,online,configured_capabilities) VALUES($1,$2,$1,'linux','amd64','/workspace','online',true,ARRAY['preview_launch']::text[])`, machine, account)
	// Another account's tunnel policy and this account's relay policy must
	// not restrict its managed preview URLs.
	exec(t, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode) VALUES('account',$1,'tunnel','self-hosted-only'),('account',$2,'relay','self-hosted-only')`, other, account)
	for index, mode := range []string{"unset", "managed-only", "self-hosted-only"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "unset" {
				exec(t, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode) VALUES('account',$1,'tunnel',$2) ON CONFLICT(scope_kind,scope_id,capability) DO UPDATE SET mode=EXCLUDED.mode`, account, mode)
			}
			id := fmt.Sprintf("%s_%d", suffix, index)
			now := time.Now().UTC()
			result, err := store.CreatePreviewLeaseV1(ctx, CreatePreviewLeaseV1Input{
				OperationID: "op_pool_" + id, LeaseID: "prv_pool_" + id, AuditEventID: "aud_pool_" + id,
				AccountID: account, ActorID: account, ActorType: "user", OwnerMachineID: machine, OwnerSessionID: "session_" + id, OwnerSessionKind: "foreground",
				TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://pool-" + fmt.Sprintf("%s-%d", suffix, index) + ".preview.example.test", LeaseDeadline: now.Add(time.Minute),
				RequestHash: sha256Bytes(id), IdempotencyKey: "pool-create:" + id, CorrelationID: "cor_pool_" + id,
				RequestID: "req_pool_" + id, SourceMachineID: machine, Now: now,
			})
			if mode == "self-hosted-only" {
				if !errors.Is(err, ErrSelfhostPreviewDomainRequired) {
					t.Fatalf("self-hosted-only admission error = %v", err)
				}
				var persisted bool
				if err := database.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.operations WHERE id=$1) OR EXISTS(SELECT 1 FROM paperboat.preview_leases WHERE id=$2)`, "op_pool_"+id, "prv_pool_"+id).Scan(&persisted); err != nil || persisted {
					t.Fatalf("denied request persisted=%v: %v", persisted, err)
				}
			} else if err != nil || result.Lease.ID != "prv_pool_"+id || result.Operation.State != "running" {
				t.Fatalf("managed preview admission: lease=%s state=%s err=%v", result.Lease.ID, result.Operation.State, err)
			}
		})
	}
}
