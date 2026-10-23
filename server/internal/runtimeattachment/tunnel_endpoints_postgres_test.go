package runtimeattachment

import (
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/endpoints"
)

// PLAN 3a step 4.1: a tunnel moves onto the endpoint engine only when its agent and every edge the
// agent holds carriers to serve tunnel endpoints, and only while all its routes are HTTP; its route
// is then assigned to each edge over the agent's runtime carrier, never as a preview admission.
func TestTunnelsMoveOntoTheEngineOnlyWhenAgentAndEdgesServeThemOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "tunnelep")
	tunnel, route, tcpRoute := "tunnel_"+f.prefix, "route_"+f.prefix, "route_tcp_"+f.prefix
	endpointID := uuid.NewString()
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+endpointID, "https://"+endpointID+".tunnels.example.test", f.machine)
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web',1,'http','managed',$4,'http','127.0.0.1:3000','not_applicable',3,'active',$3,$3,now(),now())`, route, tunnel, f.account, endpointID+".tunnels.example.test")
	sync := func() {
		t.Helper()
		if err := endpoints.SyncTunnelAgents(f.ctx, f.database, 100); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (endpoint bool, routes []string, assignments int) {
		t.Helper()
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT EXISTS(SELECT 1 FROM endpoints WHERE tunnel_id=$1 AND kind='tunnel' AND agent_machine_id=$2)`, tunnel, f.machine).Scan(&endpoint); err != nil {
			t.Fatal(err)
		}
		rows, err := f.database.SQL().QueryContext(f.ctx, `SELECT id||'@'||revision FROM endpoint_routes WHERE endpoint_id=$1 ORDER BY id`, tunnel)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			routes = append(routes, r)
		}
		rows.Close()
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM endpoint_edge_assignments WHERE endpoint_id=$1 AND desired='active'`, tunnel).Scan(&assignments); err != nil {
			t.Fatal(err)
		}
		return endpoint, routes, assignments
	}
	both := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityTunnelEndpoints}
	attach := func(capabilities []string) runtimewire.HostAdmission {
		t.Helper()
		request := f.request
		request.Capabilities = capabilities
		a, err := f.service.Attach(f.ctx, f.proof, request)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	// An agent that serves previews only keeps its tunnel on the connector.
	attach([]string{runtimewire.CapabilityEndpoints})
	sync()
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("tunnel moved for an agent without tunnel endpoints")
	}
	// The agent serves tunnels, but its edges only serve previews: still the connector.
	if _, err := f.service.Pull(f.ctx, f.edge, []string{runtimewire.CapabilityEndpoints}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(f.ctx, f.backupEdge, []string{runtimewire.CapabilityEndpoints}); err != nil {
		t.Fatal(err)
	}
	if a := attach(both); a.TunnelEndpointsServed || !a.EndpointsServed {
		t.Fatalf("served before the edges serve tunnels: endpoints=%v tunnels=%v", a.EndpointsServed, a.TunnelEndpointsServed)
	}
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("tunnel moved before the edges serve tunnel endpoints")
	}
	// One edge serves tunnels, the other not yet: still the connector.
	if _, err := f.service.Pull(f.ctx, f.edge, both); err != nil {
		t.Fatal(err)
	}
	if a := attach(both); a.TunnelEndpointsServed {
		t.Fatal("served while one edge does not serve tunnel endpoints")
	}
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("tunnel moved while one edge does not serve tunnel endpoints")
	}
	// Both edges serve tunnels: the tunnel is an endpoint with its route on both edges.
	if _, err := f.service.Pull(f.ctx, f.backupEdge, both); err != nil {
		t.Fatal(err)
	}
	carrier := attach(both)
	if !carrier.TunnelEndpointsServed {
		t.Fatal("tunnel endpoints not served with agent and both edges serving them")
	}
	if endpoint, routes, assignments := state(); !endpoint || len(routes) != 1 || routes[0] != route+"@3" || assignments != 2 {
		t.Fatalf("engine tunnel = %v %v %d, want endpoint with %s@3 on both edges", endpoint, routes, assignments, route)
	}
	// Each edge gets the route in its tunnel route snapshot, bound to the agent's runtime carrier on
	// that edge (its identity exactly as the edge admitted the carrier), not to a tunnel connector.
	control := controlplane.NewEdgeService(f.database, "")
	snapshot := func() []dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Row {
		t.Helper()
		rows, err := control.ListTunnelEdgeRouteAssignmentsForNodeV1(f.ctx, f.node, f.edge.ProcessEpoch)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	edgeCarrier := carrier.Admission
	if carrier.Backup != nil && carrier.Backup.Binding.EdgeNodeID == f.node {
		edgeCarrier = *carrier.Backup
	}
	if edgeCarrier.Binding.EdgeNodeID != f.node {
		t.Fatalf("no carrier to %s in %+v", f.node, carrier)
	}
	rows := snapshot()
	if len(rows) != 1 {
		t.Fatalf("edge snapshot has %d rows, want the engine tunnel route", len(rows))
	}
	row := rows[0]
	b := edgeCarrier.Binding
	if row.AssignmentID != "epa_"+route || row.RouteID != route || row.RouteGeneration != 3 || row.Kind != "tunnel_http_wss" || row.Protocol != "http" ||
		row.TunnelID != b.TunnelID || row.ConnectorID != b.ConnectorID || row.HostID != f.machine || row.ConnectorSessionID != b.SessionID ||
		row.ConnectorProcessGeneration != int64(b.ProcessGeneration) || row.ConfigGeneration != int64(b.ConfigGeneration) ||
		"sha256:"+hex.EncodeToString(row.ConfigContentHash) != edgeCarrier.ConfigContentHash || row.MachineIdentityPublicKey != f.key ||
		row.PublicHost != endpointID+".tunnels.example.test" || row.State != "active" || row.ObservedState != "pending" {
		t.Fatalf("engine snapshot row = %+v, carrier %+v", row, edgeCarrier)
	}
	if other, err := control.ListTunnelEdgeRouteAssignmentsForNodeV1(f.ctx, f.node, "epoch_other"); err != nil || len(other) != 0 {
		t.Fatalf("another process epoch of the edge got the route: %d %v", len(other), err)
	}
	// The edge's ready observation marks the assignment ready; a stale one is dropped.
	observe := func(generation int64, session string) error {
		return control.ObserveRoutes(f.ctx, f.node, []controlplane.RouteObservation{{
			RouteID: route, AssignmentID: row.AssignmentID, AssignmentGeneration: generation, RouteRevision: row.RouteRevision,
			EdgeNodeID: f.node, EdgeProcessEpoch: f.edge.ProcessEpoch, ConnectorID: row.ConnectorID, HostID: row.HostID,
			ConnectorGeneration: 1, ConnectorSessionID: session, ConnectorProcessGeneration: row.ConnectorProcessGeneration,
			ConfigGeneration: row.ConfigGeneration, ConfigContentHash: "sha256:" + hex.EncodeToString(row.ConfigContentHash),
			AccessMode: row.AccessMode, ViewerPolicyGeneration: row.ViewerPolicyGeneration, ObservedState: "ready",
		}})
	}
	if err := observe(row.AssignmentGeneration+1, row.ConnectorSessionID); err != nil {
		t.Fatalf("stale engine observation failed the report: %v", err)
	}
	if rows := snapshot(); rows[0].ObservedState != "pending" {
		t.Fatal("stale engine observation was applied")
	}
	if err := observe(row.AssignmentGeneration, row.ConnectorSessionID); err != nil {
		t.Fatal(err)
	}
	if rows := snapshot(); rows[0].ObservedState != "ready" {
		t.Fatalf("engine route after the edge observed it ready = %s", rows[0].ObservedState)
	}
	// A live machine has a current control session, and its environment is its own workspace.
	f.exec(t, `UPDATE control_environments SET workspace_id=$2 WHERE id=$1`, f.env, f.machine)
	f.exec(t, `INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$1,now()-interval '1 minute',now()+interval '1 hour',1)`, "renew_"+f.prefix, f.machine)
	f.exec(t, `INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, "renew_"+f.prefix)
	// The edge's public ingress authority for the route is bound to the same carrier identity its
	// stream preface carries, and names the tunnel route and its origin.
	decisions, err := connectorprotocol.SQLIngressSource{DB: f.database}.Edge(f.ctx, f.node, f.edge.ProcessEpoch)
	if err != nil {
		t.Fatal(err)
	}
	var decision *wire.IngressDecision
	for i := range decisions {
		if decisions[i].Binding.RouteID == route {
			decision = &decisions[i]
		}
	}
	if decision == nil || decision.Binding.TunnelID != b.TunnelID || decision.ConnectorID != b.ConnectorID || decision.SessionID != b.SessionID ||
		decision.ProcessGeneration != b.ProcessGeneration || decision.ConfigGeneration != b.ConfigGeneration || decision.AssignmentGeneration != uint64(row.AssignmentGeneration) ||
		decision.Binding.Hostname != endpointID+".tunnels.example.test" || decision.Binding.OriginAddress != "127.0.0.1:3000" || decision.Binding.Audience != "public" {
		t.Fatalf("engine ingress decision = %+v (of %d)", decision, len(decisions))
	}
	// Tunnel routes reach the edge with its route snapshot, never as preview admissions.
	values, err := f.service.Pull(f.ctx, f.edge, both)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range values {
		if v.EndpointID == tunnel || v.Binding.RouteID == route {
			t.Fatalf("tunnel route sent as a runtime admission: %+v", v)
		}
	}
	// A route edit follows its generation.
	f.exec(t, `UPDATE tunnel_routes SET generation=4 WHERE id=$1`, route)
	sync()
	if _, routes, _ := state(); len(routes) != 1 || routes[0] != route+"@4" {
		t.Fatalf("route revision after an edit = %v", routes)
	}
	// A private TCP route keeps the whole tunnel on its connector until TCP rides on agent carriers.
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'db',2,'private_tcp','catch_all','tcp','127.0.0.1:5432','not_applicable',1,'active',$3,$3,now(),now())`, tcpRoute, tunnel, f.account)
	sync()
	if endpoint, _, assignments := state(); endpoint || assignments != 0 {
		t.Fatal("a tunnel with a private TCP route moved onto the engine")
	}
	f.exec(t, `UPDATE tunnel_routes SET desired_state='deleted',deleted_at=now() WHERE id=$1`, tcpRoute)
	sync()
	if endpoint, routes, _ := state(); !endpoint || len(routes) != 1 {
		t.Fatal("tunnel did not return to the engine once its TCP route was removed")
	}
	// A deleted tunnel leaves the engine, and so do the tunnels of an agent that stops serving them.
	f.exec(t, `UPDATE tunnels SET desired_state='deleted',deleted_at=now() WHERE id=$1`, tunnel)
	sync()
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("deleted tunnel still on the engine")
	}
	f.exec(t, `UPDATE tunnels SET desired_state='active',deleted_at=NULL WHERE id=$1`, tunnel)
	sync()
	if endpoint, _, _ := state(); !endpoint {
		t.Fatal("restored tunnel not back on the engine")
	}
	attach([]string{runtimewire.CapabilityEndpoints})
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("tunnel stayed on the engine after its agent stopped serving tunnels")
	}
}
