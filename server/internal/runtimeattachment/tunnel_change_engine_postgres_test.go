package runtimeattachment

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

// engineTunnelOnBothEdges is an agent fixture whose tunnel is served by both edges.
func engineTunnelOnBothEdges(t *testing.T) (f *agentFixture, tunnel string, assignments func() int, repo *tunnelv1.SQLRepository) {
	t.Helper()
	f = newAgentFixture(t, "tunnelch")
	tunnel = "tunnel_" + f.prefix
	route := "route_" + f.prefix
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
	assignments = func() (n int) {
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
	return f, tunnel, assignments, repo
}

// Pausing and resuming an engine tunnel reaches the engine in the request's own transaction: the
// pause operation does not wait for the agent's next attachment check (pb tunnel pause --wait).
func TestTunnelPauseAndResumeReachTheEngineAtOnceOnPostgres(t *testing.T) {
	f, tunnel, assignments, repo := engineTunnelOnBothEdges(t)
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

// pb tunnel pause --wait returns only once every live edge has dropped the route: the operation
// completes after each edge fetched a route snapshot that began after the pause.
func TestTunnelPauseCompletesOnlyAfterEveryLiveEdgeFetchedTheSnapshotOnPostgres(t *testing.T) {
	f, tunnel, _, repo := engineTunnelOnBothEdges(t)
	control := controlplane.NewEdgeService(f.database, "")
	hash := sha256.Sum256([]byte("pause"))
	result, err := repo.Transition(f.ctx, tunnelv1.StateRecord{OperationID: "op_pause" + f.prefix, AuditEventID: "aud_pause" + f.prefix,
		TunnelID: tunnel, AccountID: f.account, DesiredState: "paused", ExpectedGeneration: 1, IdempotencyKey: "pause" + f.prefix,
		RequestHash: hash, ActorID: f.account, ActorType: "user", RequestID: "req_pause" + f.prefix, CorrelationID: "corr_" + f.prefix})
	if err != nil {
		t.Fatal(err)
	}
	state := func(want string) {
		t.Helper()
		if err := tunnelv1.RefreshHealth(f.ctx, f.database.Pool(), f.account, []string{tunnel}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT state FROM operations WHERE id=$1`, result.Operation.ID).Scan(&got); err != nil || got != want {
			t.Fatalf("pause operation = %q, %v; want %q", got, err, want)
		}
	}
	fetch := func(node string, edge previewattachment.PreviewEdgeIdentity) {
		t.Helper()
		if _, err := control.CanonicalRouteSnapshot(f.ctx, node, edge.ProcessEpoch); err != nil {
			t.Fatal(err)
		}
	}
	state("running") // no edge has fetched a snapshot since the pause
	time.Sleep(1100 * time.Millisecond)
	fetch(f.node, f.edge)
	state("running") // the other edge still serves the route
	fetch(f.backupNode, f.backupEdge)
	state("succeeded")
}
