package runtimeattachment

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

func TestTunnelPatchWaitsForAssignmentWritePostgres(t *testing.T) {
	f := newAgentFixture(t, "sync_concurrent")
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	tunnel, route := "tunnel_"+f.prefix, "route_"+f.prefix
	endpoint := uuid.NewString()
	host := endpoint + ".tunnels.example.test"
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id)
VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`, tunnel, f.account, "endpoint_"+endpoint, "https://"+host, f.machine)
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
VALUES($1,$2,'web',1,'http','managed',$4,'http','127.0.0.1:3000','not_applicable',1,'active',$3,$3,now(),now())`, route, tunnel, f.account, host)
	f.exec(t, `INSERT INTO tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until)
VALUES($1,1,sha256(convert_to($1::text,'UTF8')),convert_to(json_build_object('routes',json_build_array(json_build_object('id',$2::text)))::text,'UTF8'),'pending',$3,now()+interval '1 day')`, tunnel, route, f.account)
	request := f.request
	request.Capabilities = []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}
	if _, err := f.service.Pull(ctx, f.edge, request.Capabilities); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(ctx, f.backupEdge, request.Capabilities); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Attach(ctx, f.proof, request); err != nil {
		t.Fatal(err)
	}

	writer, err := f.database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	tag, err := writer.Exec(ctx, `UPDATE endpoint_edge_assignments SET changed_at=changed_at WHERE route_id=$1 AND edge_node_id=$2`, route, f.node)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write assignment: %d rows, %v", tag.RowsAffected(), err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	repo, err := tunnelv1.NewRepository(f.database)
	if err != nil {
		t.Fatal(err)
	}
	name := "patched-tunnel"
	input := tunnelv1.PatchRecord{
		OperationID: "patch_" + f.prefix, AuditEventID: "audit_" + f.prefix, TunnelID: tunnel, AccountID: f.account,
		Name: &name, ExpectedGeneration: 1, IdempotencyKey: f.prefix, RequestHash: sha256.Sum256([]byte(f.prefix)),
		ActorID: f.account, ActorType: "user", CorrelationID: "corr_" + f.prefix,
	}
	before := f.database.TransactionStats()
	done := make(chan error, 1)
	go func() {
		_, err := repo.Patch(ctx, input)
		done <- err
	}()
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("patch finished before the assignment write committed: %v", err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := f.database.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := f.database.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("patch retried %d transaction attempts (40001/40P01)", n)
	}
	var active int
	if err := f.database.Pool().QueryRow(ctx, `SELECT count(*) FROM endpoint_edge_assignments WHERE route_id=$1 AND desired='active'`, route).Scan(&active); err != nil || active != 2 {
		t.Fatalf("active assignments = %d, want 2, error %v", active, err)
	}
}
