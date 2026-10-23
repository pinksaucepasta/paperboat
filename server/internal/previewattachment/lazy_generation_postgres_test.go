package previewattachment_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat/server/internal/browseringress"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
)

func TestLazyBrowserReadinessGenerationPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	database, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	f, e := previewattachment.NewLazyRuntimeFixture(ctx, database, suffix, "lazy-gen-"+suffix, "session_01234567-89ab-4cde-8fab-0123456789ab")
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	defer func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM lazy_activations WHERE policy_id=$1`, "lazy-gen-"+f.Suffix)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM lazy_access_policies WHERE id=$1`, "lazy-gen-"+f.Suffix)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM usage_accounting_authorities WHERE environment_id=$1`, f.EnvironmentID)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM billing_workspaces WHERE user_id=$1`, f.AccountID)
		// The real readiness audit is append-only; its actor remains until the
		// isolated database is dropped. Remove all task forwarding resources now.
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM preview_leases WHERE account_id=$1`, f.AccountID)
		if _, e := database.SQL().ExecContext(context.Background(), `DELETE FROM user_machines WHERE user_id=$1`, f.AccountID); e != nil {
			t.Error(e)
		}
		if _, e := database.SQL().ExecContext(context.Background(), `DELETE FROM control_tunnel_nodes WHERE id=$1`, f.NodeID); e != nil {
			t.Error(e)
		}
	}()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := database.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	policy, boot := "lazy-gen-"+f.Suffix, "boot-lazy-generation-"+f.Suffix
	host := f.LazyHostname
	exec(`UPDATE preview_leases SET access_mode='private' WHERE id=$1`, f.PreviewID(0))
	exec(`INSERT INTO lazy_runtime_owners(machine_id,account_id,installation_generation,boot_id,started_at,expires_at) VALUES($1,$2,1,$3,$4,$5)`, f.MachineID, f.AccountID, boot, now, now.Add(time.Minute))
	exec(`INSERT INTO lazy_activations(policy_id,policy_generation,boot_id,activation_id,deadline,preview_id,state,cooldown_until,owner_session_id) VALUES($1,1,$2,$3,$4,$5,'ready',$4,'session_01234567-89ab-4cde-8fab-0123456789ab')`, policy, boot, "activation-"+f.Suffix, now.Add(time.Minute), f.PreviewID(0))
	repo, _ := previewattachment.NewSQLRepository(database)
	attachment := f.Attachment(f.PreviewID(0), f.OperationID(0), f.OwnerSessionID)
	attachment.AccessMode = "private"
	attachment.Endpoint = "https://" + host
	if _, e = repo.CreatePending(ctx, attachment); e != nil {
		t.Fatal(e)
	}
	exec(`UPDATE preview_lease_carrier_attachments SET state='ready',edge_ready=true,origin_ready=true,ready_at=now() WHERE preview_id=$1`, attachment.PreviewID)
	store, _ := previewtunnelstore.New(database)
	ready, e := store.MarkPreviewLeaseReadyV1(ctx, previewtunnelstore.MarkPreviewLeaseReadyV1Input{AuditEventID: "audit-ready-" + f.Suffix, AccountID: f.AccountID, ActorID: f.AccountID, ActorType: "user", PreviewID: attachment.PreviewID, ExpectedGeneration: 1, AllocationState: "ready", EdgeState: "ready", OriginState: "ready", RequestID: "request-" + f.Suffix, CorrelationID: "correlation-" + f.Suffix, Now: now})
	if e != nil || ready.Generation != 2 {
		t.Fatalf("real readiness CAS: generation=%d error=%v", ready.Generation, e)
	}
	resolve := func() error {
		var id, route string
		var gen int64
		var expires time.Time
		return database.SQL().QueryRowContext(ctx, browseraccess.LazyPreviewSQL, policy, 1, attachment.PreviewID, "", time.Now().UTC()).Scan(&id, &route, &gen, &expires)
	}
	if e = resolve(); e != nil {
		t.Fatalf("ready CAS invalidated admitted immutable attachment: %v", e)
	}

	exec(`INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$1,$3,$4,1)`, "renew-lazy-"+f.Suffix, f.MachineID, now.Add(-time.Minute), now.Add(time.Hour))
	exec(`INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, "renew-lazy-"+f.Suffix)
	ingress := browseringress.Service{DB: database}
	admitted := browseraccess.Authorization{AccountID: f.AccountID, OwnerAccountID: f.AccountID, ResourceKind: "lazy_policy", ResourceID: policy, ResourceGeneration: 1, RouteID: policy, RouteGeneration: 1, Hostname: host, AccessMode: "private", GrantID: "grant-lazy-" + f.Suffix, GrantGeneration: 1, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if _, e = ingress.Resolve(ctx, admitted, f.NodeID, f.Epoch); e != nil {
		t.Fatalf("ready CAS invalidated actual ingress consumer: %v", e)
	}
	exec(`UPDATE preview_lease_carrier_attachments SET lease_generation=3 WHERE preview_id=$1`, attachment.PreviewID)
	if resolve() == nil {
		t.Fatal("future attachment generation accepted")
	}
	exec(`UPDATE preview_lease_carrier_attachments SET lease_generation=1 WHERE preview_id=$1`, attachment.PreviewID)
	exec(`UPDATE preview_leases SET target_address='127.0.0.1:3999' WHERE id=$1`, attachment.PreviewID)
	if resolve() == nil {
		t.Fatal("changed target accepted")
	}
	exec(`UPDATE preview_leases SET target_address='127.0.0.1:3000' WHERE id=$1`, attachment.PreviewID)
	if _, e = database.SQL().ExecContext(ctx, `UPDATE preview_lease_carrier_attachments SET operation_id=$2 WHERE preview_id=$1`, attachment.PreviewID, f.OperationID(1)); e == nil {
		t.Fatal("unrelated operation accepted")
	}
	if e = resolve(); e != nil {
		t.Fatalf("exact binding did not recover: %v", e)
	}
}
