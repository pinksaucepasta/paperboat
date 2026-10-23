package runtimeattachment

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
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
	f.exec(t, `INSERT INTO tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until)
VALUES($1,1,sha256(convert_to($1::text,'UTF8')),convert_to(json_build_object('routes',json_build_array(json_build_object('id',$2::text)))::text,'UTF8'),'pending',$3,now()+interval '1 day')`, tunnel, route, f.account)
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
	// A private tunnel keeps its connector until the agent has its own ingress authority for engine
	// routes; back to public, it moves again.
	f.exec(t, `UPDATE tunnels SET access_mode='private' WHERE id=$1`, tunnel)
	sync()
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("a private tunnel moved onto the engine")
	}
	f.exec(t, `UPDATE tunnels SET access_mode='public' WHERE id=$1`, tunnel)
	sync()
	if endpoint, routes, assignments := state(); !endpoint || len(routes) != 1 || assignments != 2 {
		t.Fatalf("public again: engine tunnel = %v %v %d", endpoint, routes, assignments)
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
	// The edge's durable admission registry admits a replica only when the row's thumbprint is the
	// connector (JWK) thumbprint of its machine key; the runtime carrier's own sha256 thumbprint is
	// another format (round 21: every engine route stayed unobserved). Check the snapshot the edge
	// receives, and keep it as the golden contract the edge's own test consumes.
	items, err := control.CanonicalRouteSnapshot(f.ctx, f.node, f.edge.ProcessEpoch)
	if err != nil || len(items) != 1 {
		t.Fatalf("canonical snapshot: %d %v", len(items), err)
	}
	key, err := base64.RawURLEncoding.DecodeString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	if want, err := wire.IdentityThumbprint(ed25519.PublicKey(key)); err != nil || items[0]["machine_identity_thumbprint"] != want {
		t.Fatalf("engine route thumbprint = %v, want the connector thumbprint %q (%v)", items[0]["machine_identity_thumbprint"], want, err)
	}
	checkEngineRouteGolden(t, items[0])
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
	// The tunnel is ready only once every edge holding its route has observed it (F8-06).
	health := func() string {
		t.Helper()
		var code string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT summary_code FROM tunnels WHERE id=$1`, tunnel).Scan(&code); err != nil {
			t.Fatal(err)
		}
		return code
	}
	if code := health(); code != "pending" {
		t.Fatalf("tunnel health with one of two edges ready = %s, want pending", code)
	}
	backupRows, err := control.ListTunnelEdgeRouteAssignmentsForNodeV1(f.ctx, f.backupNode, f.backupEdge.ProcessEpoch)
	if err != nil || len(backupRows) != 1 {
		t.Fatalf("backup edge snapshot = %d rows, %v", len(backupRows), err)
	}
	backup := backupRows[0]
	if err := control.ObserveRoutes(f.ctx, f.backupNode, []controlplane.RouteObservation{{
		RouteID: route, AssignmentID: backup.AssignmentID, AssignmentGeneration: backup.AssignmentGeneration, RouteRevision: backup.RouteRevision,
		EdgeNodeID: f.backupNode, EdgeProcessEpoch: f.backupEdge.ProcessEpoch, ConnectorID: backup.ConnectorID, HostID: backup.HostID,
		ConnectorGeneration: 1, ConnectorSessionID: backup.ConnectorSessionID, ConnectorProcessGeneration: backup.ConnectorProcessGeneration,
		ConfigGeneration: backup.ConfigGeneration, ConfigContentHash: "sha256:" + hex.EncodeToString(backup.ConfigContentHash),
		AccessMode: backup.AccessMode, ViewerPolicyGeneration: backup.ViewerPolicyGeneration, ObservedState: "ready",
	}}); err != nil {
		t.Fatal(err)
	}
	// A live machine has a current control session, and its environment is its own workspace.
	f.exec(t, `UPDATE control_environments SET workspace_id=$2 WHERE id=$1`, f.env, f.machine)
	f.exec(t, `INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$1,now()-interval '1 minute',now()+interval '1 hour',1)`, "renew_"+f.prefix, f.machine)
	f.exec(t, `INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, "renew_"+f.prefix)
	if err := tunnelv1.RefreshHealth(f.ctx, f.database.Pool(), f.account, []string{tunnel}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if code := health(); code != "ready" {
		t.Fatalf("tunnel health with both edges ready = %s, want ready", code)
	}
	// No connector applies an engine tunnel's config: its newest generation is active.
	var activation string
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT activation_state FROM tunnel_config_generations WHERE tunnel_id=$1 AND generation=1`, tunnel).Scan(&activation); err != nil || activation != "active" {
		t.Fatalf("engine tunnel config = %q, %v; want active", activation, err)
	}
	// The agent learns what to serve (routes and origins) from its engine tunnels' active configs.
	served, err := f.service.Tunnels(f.ctx, f.proof, runtimewire.TunnelsRequest{OperationID: f.proof.OperationID})
	if err != nil || len(served) != 1 || served[0].TunnelID != tunnel || served[0].ConfigGeneration != 1 || !strings.Contains(string(served[0].Snapshot), route) {
		t.Fatalf("agent engine tunnels = %+v, %v", served, err)
	}
	if _, err := f.service.Tunnels(f.ctx, f.proof, runtimewire.TunnelsRequest{OperationID: "other"}); err != ErrInvalid {
		t.Fatalf("tunnels request not bound to its proof: %v", err)
	}
	// Its hostname's DNS names exactly the edges that serve the route.
	f.exec(t, `UPDATE control_tunnel_nodes SET public_ingress_ipv4=CASE WHEN id=$1 THEN '192.0.2.21'::inet ELSE '192.0.2.22'::inet END,public_ingress_verified_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode)
	certificate := "cert_" + f.prefix
	f.exec(t, `INSERT INTO tunnel_platform_certificate_targets(id,kind,hostname,account_id,challenge_reference) VALUES('platform_cert_public_v1','public_wildcard','*.tunnels.example.test','platform_system_v1','dns:fixture')`)
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM tunnel_platform_certificate_targets WHERE id='platform_cert_public_v1'`)
	})
	f.exec(t, `INSERT INTO tunnel_certificate_records(id,domain_id,account_id,hostname,domain_generation,certificate_generation,strategy,state,certificate_reference,master_key_reference,certificate_ciphertext,private_key_ciphertext,fingerprint,issuer,not_before,expires_at,renewal_at,target_kind) VALUES($1,'platform_cert_public_v1','platform_system_v1','*.tunnels.example.test',1,1,'platform_dns01','active','fixture','fixture',decode(repeat('ab',29),'hex'),decode(repeat('ab',29),'hex'),decode(repeat('ab',32),'hex'),'fixture',now()-interval '1 hour',now()+interval '1 hour',now(),'platform_wildcard')`, certificate)
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM tunnel_certificate_records WHERE id=$1`, certificate)
	})
	f.exec(t, `INSERT INTO tunnel_certificate_edge_distributions(certificate_id,edge_node_id,edge_process_epoch,edge_assignment_generation,state,observed_certificate_generation) VALUES($1,$2,$3,1,'active',1),($1,$4,$5,1,'active',1)`, certificate, f.node, f.edge.ProcessEpoch, f.backupNode, f.backupEdge.ProcessEpoch)
	addresses, err := f.database.Queries().ListReadyEdgeDNSAddressesV1(f.ctx, dbsqlc.ListReadyEdgeDNSAddressesV1Params{TunnelID: tunnel, Hostname: endpointID + ".tunnels.example.test", Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
	if err != nil || len(addresses) != 2 {
		t.Fatalf("engine tunnel DNS addresses = %+v, %v; want both edges", addresses, err)
	}
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

// engineRouteGolden is the engine route row the server sends an edge, kept where the edge's test
// reads it (tunnel/internal/runtime: TestEdgeAdmitsTheServersEngineRouteGolden), so both sides run
// their real code against one contract. PAPERBOAT_UPDATE_GOLDEN=<path of that file> rewrites it.
const engineRouteGolden = "../../../shared/runtimeattachment/testdata/engine_route_snapshot.json"

func checkEngineRouteGolden(t *testing.T, item map[string]any) {
	t.Helper()
	encoded, err := json.MarshalIndent(map[string]any{"complete": true, "routes": []any{item}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("PAPERBOAT_UPDATE_GOLDEN"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(engineRouteGolden)
	if err != nil {
		t.Fatalf("read the engine route golden (PAPERBOAT_UPDATE_GOLDEN=<path> writes it): %v", err)
	}
	var golden struct {
		Routes []map[string]any `json:"routes"`
	}
	var current struct {
		Routes []map[string]any `json:"routes"`
	}
	if json.Unmarshal(raw, &golden) != nil || json.Unmarshal(encoded, &current) != nil || len(golden.Routes) != 1 {
		t.Fatal("engine route golden unreadable")
	}
	for field := range current.Routes[0] {
		if _, ok := golden.Routes[0][field]; !ok {
			t.Fatalf("server sends field %q the golden lacks: rewrite it with PAPERBOAT_UPDATE_GOLDEN and run the edge test", field)
		}
	}
	for field := range golden.Routes[0] {
		if _, ok := current.Routes[0][field]; !ok {
			t.Fatalf("golden field %q is no longer sent: rewrite it with PAPERBOAT_UPDATE_GOLDEN and run the edge test", field)
		}
	}
}
