package runtimeattachment

import (
	"testing"
	"time"

	"github.com/google/uuid"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
)

// A tunnel assignment change reaches the edge's fast endpoint pull as TunnelsChangedAt, once per
// change: the edge wakes its route worker on a new value, so a created tunnel is served within a
// second instead of at the control interval. Rows older than the delta overlap and full answers
// carry none.
func TestEndpointDeltaReportsTunnelAssignmentChangesOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "tunnelchg")
	tunnel, route := "tunnel_"+f.prefix, "route_"+f.prefix
	endpointID := uuid.NewString()
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+endpointID, "https://"+endpointID+".tunnels.example.test", f.machine)
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web',1,'http','managed',$4,'http','127.0.0.1:3000','not_applicable',3,'active',$3,$3,now(),now())`, route, tunnel, f.account, endpointID+".tunnels.example.test")
	f.exec(t, `INSERT INTO tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until)
VALUES($1,1,sha256(convert_to($1::text,'UTF8')),convert_to(json_build_object('routes',json_build_array(json_build_object('id',$2::text)))::text,'UTF8'),'pending',$3,now()+interval '1 day')`, tunnel, route, f.account)
	both := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints, runtimewire.CapabilityEndpointDeltas}
	if _, err := f.service.Pull(f.ctx, f.edge, both); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(f.ctx, f.backupEdge, both); err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.Capabilities = both[:2]
	if a, err := f.service.Attach(f.ctx, f.proof, request); err != nil || !a.TunnelEndpointsServed {
		t.Fatalf("attach: tunnels served=%v %v", a.TunnelEndpointsServed, err)
	}
	full, err := f.service.PullEndpoints(f.ctx, f.edge, both, "", "", 0)
	if err != nil || !full.Complete || full.TunnelsChangedAt != "" {
		t.Fatalf("full pull = %+v, %v", full, err)
	}
	if err := endpoints.SyncTunnelAgents(f.ctx, f.database, 100); err != nil {
		t.Fatal(err)
	}
	delta, err := f.service.PullEndpoints(f.ctx, f.edge, both, full.Cursor, "", 0)
	if err != nil || delta.Complete || delta.TunnelsChangedAt == "" {
		t.Fatalf("delta after the tunnel was assigned = %+v, %v", delta, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, delta.TunnelsChangedAt); err != nil {
		t.Fatalf("tunnels_changed_at %q: %v", delta.TunnelsChangedAt, err)
	}
	again, err := f.service.PullEndpoints(f.ctx, f.edge, both, delta.Cursor, "", 0)
	if err != nil || again.TunnelsChangedAt != delta.TunnelsChangedAt {
		t.Fatalf("a repeated delta changed the value: %q vs %q (%v)", again.TunnelsChangedAt, delta.TunnelsChangedAt, err)
	}
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	idle, err := f.service.PullEndpoints(f.ctx, f.edge, both, delta.Cursor, "", 0)
	if err != nil || idle.TunnelsChangedAt != "" {
		t.Fatalf("idle delta = %+v, %v", idle, err)
	}
}
