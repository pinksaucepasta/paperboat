package runtimeattachment

import (
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

// Pausing and resuming an engine tunnel reaches the engine in the request's own transaction: the
// pause operation does not wait for the agent's next attachment check (pb tunnel pause --wait).
func TestTunnelPauseAndResumeReachTheEngineAtOnceOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "tunnelch")
	tunnel, route := "tunnel_"+f.prefix, "route_"+f.prefix
	endpointID := uuid.NewString()
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+endpointID, "https://"+endpointID+".tunnels.example.test", f.machine)
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web',1,'http','managed',$4,'http','127.0.0.1:3000','not_applicable',1,'active',$3,$3,now(),now())`, route, tunnel, f.account, endpointID+".tunnels.example.test")
	f.exec(t, `INSERT INTO tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until)
VALUES($1,1,sha256(convert_to($1::text,'UTF8')),convert_to(json_build_object('routes',json_build_array(json_build_object('id',$2::text)))::text,'UTF8'),'pending',$3,now()+interval '1 day')`, tunnel, route, f.account)
	both := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}
	if _, err := f.service.Pull(f.ctx, f.edge, both); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(f.ctx, f.backupEdge, both); err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.Capabilities = both
	if _, err := f.service.Attach(f.ctx, f.proof, request); err != nil {
		t.Fatal(err)
	}
	assignments := func() (n int) {
		t.Helper()
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM endpoint_edge_assignments WHERE endpoint_id=$1 AND desired='active'`, tunnel).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := assignments(); n != 2 {
		t.Fatalf("assignments after attach = %d, want 2", n)
	}
	repo, err := tunnelv1.NewRepository(f.database)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(1)
	for i, state := range []string{"paused", "active"} {
		hash := sha256.Sum256([]byte(state))
		result, err := repo.Transition(f.ctx, tunnelv1.StateRecord{OperationID: "op_" + state + f.prefix, AuditEventID: "aud_" + state + f.prefix,
			TunnelID: tunnel, AccountID: f.account, DesiredState: state, ExpectedGeneration: generation, IdempotencyKey: state + f.prefix,
			RequestHash: hash, ActorID: f.account, ActorType: "user", RequestID: "req_" + state + f.prefix, CorrelationID: "corr_" + f.prefix})
		if err != nil {
			t.Fatal(err)
		}
		generation = result.Tunnel.Generation
		if want := []int{0, 2}[i]; assignments() != want {
			t.Fatalf("assignments right after %s = %d, want %d", state, assignments(), want)
		}
	}
}
