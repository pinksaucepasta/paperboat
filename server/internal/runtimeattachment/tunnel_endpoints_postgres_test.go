package runtimeattachment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/ingressprojection"
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

	// The machine key comes from the shared test seed, so the edge's contract test can present a
	// carrier certificate for the golden row this test writes.
	seed, err := os.ReadFile("../../../shared/runtimeattachment/testdata/engine_machine_seed.hex")
	if err != nil {
		t.Fatal(err)
	}
	seedBytes, err := hex.DecodeString(strings.TrimSpace(string(seed)))
	if err != nil || len(seedBytes) != ed25519.SeedSize {
		t.Fatalf("engine machine seed: %v", err)
	}
	f.key = base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(seedBytes).Public().(ed25519.PublicKey))
	f.exec(t, `UPDATE user_machines SET public_identity_key=$2 WHERE id=$1`, f.machine, f.key)
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
	// A private tunnel stays on the engine (3a step 4.4: its streams carry the edge's browser
	// decision, which the agent re-checks).
	f.exec(t, `UPDATE tunnels SET access_mode='private' WHERE id=$1`, tunnel)
	sync()
	if endpoint, routes, assignments := state(); !endpoint || len(routes) != 1 || assignments != 2 {
		t.Fatalf("private: engine tunnel = %v %v %d", endpoint, routes, assignments)
	}
	f.exec(t, `UPDATE tunnels SET access_mode='public' WHERE id=$1`, tunnel)
	sync()
	if endpoint, routes, assignments := state(); !endpoint || len(routes) != 1 || assignments != 2 {
		t.Fatalf("public again: engine tunnel = %v %v %d", endpoint, routes, assignments)
	}
	// A second tunnel moves with the scoped sync a create runs: only that tunnel is synced, to the same state the full
	// sync gives it. Another tunnel's missing endpoint route (removed by hand here) stays missing until the next full
	// sync, which proves the scope; the full sync then changes nothing for the new tunnel (F21-168: the machine-wide
	// sync on every create read every tunnel and reconcile every assignment).
	tunnelTwo, routeTwo, endpointTwo := tunnel+"_two", route+"_two", uuid.NewString()
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','public',1,$3,$4,$5,$2)`,
		tunnelTwo, f.account, "endpoint_"+endpointTwo, "https://"+endpointTwo+".tunnels.example.test", f.machine)
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web',1,'http','managed',$4,'http','127.0.0.1:3000','not_applicable',3,'active',$3,$3,now(),now())`, routeTwo, tunnelTwo, f.account, endpointTwo+".tunnels.example.test")
	f.exec(t, `INSERT INTO tunnel_config_generations(tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until)
VALUES($1,1,sha256(convert_to($1::text,'UTF8')),convert_to(json_build_object('routes',json_build_array(json_build_object('id',$2::text)))::text,'UTF8'),'pending',$3,now()+interval '1 day')`, tunnelTwo, routeTwo, f.account)
	f.exec(t, `DELETE FROM endpoint_routes WHERE endpoint_id=$1`, tunnel)
	if err := f.database.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		return endpoints.SyncAgentTunnelTx(ctx, tx, f.machine, tunnelTwo)
	}); err != nil {
		t.Fatal(err)
	}
	scoped := func() string {
		t.Helper()
		var out string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT coalesce(string_agg(x.edge_node_id||'/'||x.desired||'/'||x.generation,',' ORDER BY x.edge_node_id),'') FROM endpoint_edge_assignments x WHERE x.endpoint_id=$1`, tunnelTwo).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var routesOne, routesTwo int
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM endpoint_routes WHERE endpoint_id=$1),(SELECT count(*) FROM endpoint_routes WHERE endpoint_id=$2)`, tunnel, tunnelTwo).Scan(&routesOne, &routesTwo); err != nil {
		t.Fatal(err)
	}
	afterScoped := scoped()
	if routesTwo != 1 || strings.Count(afterScoped, "/active/") != 2 {
		t.Fatalf("scoped sync of the new tunnel: %d routes, assignments %q; want 1 route on both edges", routesTwo, afterScoped)
	}
	if routesOne != 0 {
		t.Fatalf("the scoped sync of one tunnel also repaired another tunnel (%d routes): it is not scoped", routesOne)
	}
	sync()
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM endpoint_routes WHERE endpoint_id=$1`, tunnel).Scan(&routesOne); err != nil || routesOne != 1 {
		t.Fatalf("the full sync did not repair the first tunnel's route: %d, %v", routesOne, err)
	}
	if afterFull := scoped(); afterFull != afterScoped {
		t.Fatalf("the full sync changed what the scoped sync assigned: %q then %q", afterScoped, afterFull)
	}
	f.exec(t, `UPDATE tunnels SET desired_state='deleted', deleted_at=now() WHERE id=$1`, tunnelTwo)
	sync()
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
		row.TunnelID != b.TunnelID || row.ResourceID != tunnel || row.ConnectorID != b.ConnectorID || row.HostID != f.machine || row.ConnectorSessionID != b.SessionID ||
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
	// The edge reports the agent's carriers up (as it does before their routes); a route is ready on
	// an edge only while that carrier is.
	f.exec(t, `UPDATE runtime_carrier_attachments SET ready=true WHERE machine_id=$1`, f.machine)
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
	// The state change is a tunnel log line (pb tunnel logs); an unchanged repeat adds none.
	if err := observe(row.AssignmentGeneration, row.ConnectorSessionID); err != nil {
		t.Fatal(err)
	}
	repo, err := tunnelv1.NewRepository(f.database)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := repo.ListResourceTunnelLogs(f.ctx, f.account, tunnel, 0, 10)
	if err != nil || len(logs) != 1 || logs[0].Code != "tunnel_edge_ready" || logs[0].RouteID.String != route {
		t.Fatalf("engine tunnel logs = %+v, %v; want one tunnel_edge_ready for the route", logs, err)
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
	// It also carries the generations an inspector credential binds a capture to, so replay works.
	var tunnelGeneration, routeGeneration uint64
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT t.generation,r.generation FROM tunnels t JOIN tunnel_routes r ON r.tunnel_id=t.id WHERE r.id=$1`, route).Scan(&tunnelGeneration, &routeGeneration); err != nil {
		t.Fatal(err)
	}
	if served[0].TunnelGeneration != tunnelGeneration || served[0].RouteGenerations[route] != routeGeneration {
		t.Fatalf("engine tunnel generations = %d %v, want %d %s:%d", served[0].TunnelGeneration, served[0].RouteGenerations, tunnelGeneration, route, routeGeneration)
	}
	if _, err := f.service.Tunnels(f.ctx, f.proof, runtimewire.TunnelsRequest{OperationID: "other"}); err != ErrInvalid {
		t.Fatalf("tunnels request not bound to its proof: %v", err)
	}
	// Its hostname's DNS names exactly the edges that serve the route.
	f.exec(t, `UPDATE control_tunnel_nodes SET public_ingress_ipv4=CASE WHEN id=$1 THEN '192.0.2.21'::inet ELSE '192.0.2.22'::inet END,public_ingress_verified_at=now() WHERE id IN ($1,$2)`, f.node, f.backupNode)
	certificate := "cert_" + f.prefix
	// The platform target is a singleton other suites may hold in a shared database: create it only
	// if absent, and remove it only if this test created it.
	var created int64
	if err := f.database.SQL().QueryRowContext(f.ctx, `WITH inserted AS (INSERT INTO tunnel_platform_certificate_targets(id,kind,hostname,account_id,challenge_reference) VALUES('platform_cert_public_v1','public_wildcard','*.tunnels.example.test','platform_system_v1','dns:fixture') ON CONFLICT (id) DO NOTHING RETURNING 1) SELECT count(*) FROM inserted`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created == 1 {
		t.Cleanup(func() {
			_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM tunnel_platform_certificate_targets WHERE id='platform_cert_public_v1'`)
		})
	}
	f.exec(t, `INSERT INTO tunnel_certificate_records(id,domain_id,account_id,hostname,domain_generation,certificate_generation,strategy,state,certificate_reference,master_key_reference,certificate_ciphertext,private_key_ciphertext,fingerprint,issuer,not_before,expires_at,renewal_at,target_kind) VALUES($1,'platform_cert_public_v1','platform_system_v1','*.tunnels.example.test',1,1,'platform_dns01','active','fixture','fixture',decode(repeat('ab',29),'hex'),decode(repeat('ab',29),'hex'),decode(repeat('ab',32),'hex'),'fixture',now()-interval '1 hour',now()+interval '1 hour',now(),'platform_wildcard')`, certificate)
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM tunnel_certificate_records WHERE id=$1`, certificate)
	})
	f.exec(t, `INSERT INTO tunnel_certificate_edge_distributions(certificate_id,edge_node_id,edge_process_epoch,edge_assignment_generation,state,observed_certificate_generation) VALUES($1,$2,$3,1,'active',1),($1,$4,$5,1,'active',1)`, certificate, f.node, f.edge.ProcessEpoch, f.backupNode, f.backupEdge.ProcessEpoch)
	// The edge's public ingress authority for the route is bound to the same carrier identity its
	// stream preface carries, and names the tunnel route and its origin.
	decisions, err := ingressprojection.SQLIngressSource{DB: f.database, Quota: openQuota{}}.Edge(f.ctx, f.node, f.edge.ProcessEpoch)
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
	// A replacement server's warm-up projects read-only: the same decisions, and no accounting binding written, so it
	// never queues on the session lock the serving server's desired-state answer takes (deploys 2026-11-12 19:54Z/19:57Z).
	f.exec(t, `DELETE FROM usage_accounting_authorities WHERE edge_node_id=$1`, f.node)
	warm, err := ingressprojection.SQLIngressSource{DB: f.database, ReadOnly: true}.Edge(f.ctx, f.node, f.edge.ProcessEpoch)
	var bindings int
	if err == nil {
		err = f.database.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM usage_accounting_authorities WHERE edge_node_id=$1`, f.node).Scan(&bindings)
	}
	if err != nil || len(warm) != len(decisions) || bindings != 0 {
		t.Fatalf("read-only projection: %d decisions (want %d), %d accounting bindings written, %v", len(warm), len(decisions), bindings, err)
	}
	// F21-38: with nothing changed between two pulls the ingress and route answers keep the same
	// version (decision ids and times are outside it), so an edge holding it gets "unchanged".
	again, err := ingressprojection.SQLIngressSource{DB: f.database, Quota: openQuota{}}.Edge(f.ctx, f.node, f.edge.ProcessEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if first, second := wire.IngressVersion(decisions, nil, nil, nil, nil, nil), wire.IngressVersion(again, nil, nil, nil, nil, nil); len(decisions) == 0 || first != second {
		t.Fatalf("ingress version moved between idle pulls: %d decisions, %s then %s", len(decisions), first, second)
	}
	routesA, errA := control.CanonicalRouteSnapshot(f.ctx, f.node, f.edge.ProcessEpoch)
	routesB, errB := control.CanonicalRouteSnapshot(f.ctx, f.node, f.edge.ProcessEpoch)
	encodedA, _ := json.Marshal(routesA)
	encodedB, _ := json.Marshal(routesB)
	if errA != nil || errB != nil || len(routesA) == 0 || !bytes.Equal(encodedA, encodedB) {
		t.Fatalf("route answer moved between idle pulls: %v %v\n%s\n%s", errA, errB, encodedA, encodedB)
	}
	checkWorkerHandover(t, f, control, route)
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
	// A public TCP route rides along (3a step 4.4): the tunnel stays on the engine with both routes.
	publicTCP := "route_ptcp_" + f.prefix
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,public_tcp_listener_id,public_tcp_port,preserve_host,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'ssh',3,'tcp','managed',$4,'tcp','127.0.0.1:22','not_applicable',1,'active','listener_'||$1,24022,false,$3,$3,now(),now())`, publicTCP, tunnel, f.account, endpointID+".tunnels.example.test")
	sync()
	if endpoint, routes, _ := state(); !endpoint || len(routes) != 2 {
		t.Fatalf("with a public TCP route: engine tunnel = %v %v", endpoint, routes)
	}
	f.exec(t, `UPDATE tunnel_routes SET desired_state='deleted',deleted_at=now() WHERE id=$1`, publicTCP)
	sync()
	// A private TCP route is valid only on a private tunnel: a public one keeps its connector.
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'db',2,'private_tcp','catch_all','tcp','127.0.0.1:5432','not_applicable',1,'active',$3,$3,now(),now())`, tcpRoute, tunnel, f.account)
	sync()
	if endpoint, _, assignments := state(); endpoint || assignments != 0 {
		t.Fatal("a public tunnel with a private TCP route moved onto the engine")
	}
	// On a private tunnel the private TCP route rides the engine with the HTTP route.
	f.exec(t, `UPDATE tunnels SET access_mode='private' WHERE id=$1`, tunnel)
	sync()
	if endpoint, routes, _ := state(); !endpoint || len(routes) != 2 {
		t.Fatalf("private tunnel with private TCP: engine tunnel = %v %v", endpoint, routes)
	}
	f.exec(t, `UPDATE tunnels SET access_mode='public' WHERE id=$1`, tunnel)
	sync()
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
	// A custom domain on an engine tunnel gets certificate targets from the engine's ready edge assignments:
	// the certificate worker read only connector-path route assignments, so such a domain never got an
	// edge to place its certificate on (Lane M, staging 4.3).
	domain := "domain_" + f.prefix
	f.exec(t, `INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'managed','issuing','ready','clear')`, domain, f.account, tunnel, route, "custom-"+f.prefix+".example.test")
	f.exec(t, `UPDATE endpoint_edge_assignments SET observed_ready=true,observed_at=now() WHERE endpoint_id=$1 AND desired='active'`, tunnel)
	targets, err := f.database.Queries().ListReadyTunnelCertificateEdgesV1(f.ctx, domain)
	if err != nil || len(targets) == 0 {
		t.Fatalf("engine tunnel domain certificate targets = %+v, %v; want its ready engine edges", targets, err)
	}
	for _, target := range targets {
		if target.EdgeNodeID != f.edge.NodeID && target.EdgeNodeID != f.backupEdge.NodeID {
			t.Fatalf("certificate target %q is not one of the agent's edges", target.EdgeNodeID)
		}
	}
	// `pb tunnel route add --wait` on an engine tunnel ends: the route operation settles once the
	// engine's edges serve the route's current generation (it waited on connector assignments only).
	routeOperation := "op_route_" + f.prefix
	f.exec(t, `INSERT INTO operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,created_at,updated_at)
VALUES ($1,$2,$3,decode(repeat('42',32),'hex'),'route.create','route',$4,'connecting','running',50,'changed',$5,now(),now())`, routeOperation, f.account, "idem_"+routeOperation, route, "cor_"+routeOperation)
	routeStore, err := previewtunnelstore.New(f.database)
	if err != nil {
		t.Fatal(err)
	}
	if operation, err := routeStore.GetOperation(f.ctx, f.account, routeOperation); err != nil || operation.State != "succeeded" {
		t.Fatalf("engine route operation = %+v, %v; want succeeded once its engine edges serve it", operation, err)
	}
	// An operator edge's usage report for an engine route is bound to the route the edge serves, at the
	// tunnel route generation and agent environment its ingress decision carried: the usage endpoint read
	// only connector, preview and carrier paths and refused every engine report (staging, 4/s).
	var environment string
	var generation int64
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT m.environment_id,r.generation FROM user_machines m, tunnel_routes r WHERE m.id=$1 AND r.id=$2`, f.machine, route).Scan(&environment, &generation); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		route    string
		revision int64
		want     bool
	}{{route, generation, true}, {route, generation - 1, false}, {"route_not_held_" + f.prefix, generation, false}} {
		held, err := f.database.EdgeHoldsUsageRoute(f.ctx, f.edge.NodeID, c.route, c.revision, environment)
		if err != nil || held != c.want {
			t.Fatalf("usage route %s@%d held = %v, %v; want %v", c.route, c.revision, held, err, c.want)
		}
	}
	// The certificate ledger treats the engine's edges like connector edges: a domain whose active
	// certificate is not yet acknowledged by every engine edge is due, and once each edge acknowledges
	// the certificate at its engine assignment generation the domain is complete (not due again).
	certID := "tcert_" + f.prefix
	f.exec(t, `INSERT INTO tunnel_certificate_records(id,domain_id,account_id,tunnel_id,route_id,hostname,domain_generation,certificate_generation,strategy,state,certificate_reference,master_key_reference,certificate_ciphertext,private_key_ciphertext,fingerprint,issuer,not_before,expires_at,renewal_at,target_kind) VALUES($1,$2,$3,$4,$5,$6,1,1,'delegated_dns01','active',$1,'fixture',decode(repeat('ab',29),'hex'),decode(repeat('ab',29),'hex'),decode(repeat('ab',32),'hex'),'fixture',now()-interval '1 hour',now()+interval '60 days',now()+interval '30 days','durable_route')`, certID, domain, f.account, tunnel, route, "custom-"+f.prefix+".example.test")
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(f.ctx, `DELETE FROM tunnel_certificate_records WHERE id=$1`, certID)
	})
	f.exec(t, `UPDATE tunnel_domains SET certificate_state='ready',certificate_reference=$2 WHERE id=$1`, domain, certID)
	// The heartbeats the edges' eligibility checks read are fresh for this part of the test.
	f.exec(t, `UPDATE control_tunnel_nodes SET last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour' WHERE id IN ($1,$2)`, f.node, f.backupNode)
	due := func() bool {
		t.Helper()
		rows, err := f.database.Queries().ListDueTunnelCertificateDomainsV1(f.ctx, dbsqlc.ListDueTunnelCertificateDomainsV1Params{Now: time.Now().UTC(), RenewBefore: pgtype.Interval{Microseconds: int64(time.Hour / time.Microsecond)}, RowLimit: 10000})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == domain {
				return true
			}
		}
		return false
	}
	if !due() {
		t.Fatal("engine domain with an unacknowledged certificate is not due")
	}
	for _, target := range targets {
		f.exec(t, `INSERT INTO tunnel_certificate_edge_distributions(certificate_id,edge_node_id,edge_process_epoch,edge_assignment_generation,state,observed_certificate_generation) VALUES($1,$2,$3,$4,'active',1)`, certID, target.EdgeNodeID, target.EdgeProcessEpoch, target.AssignmentGeneration)
	}
	if due() {
		t.Fatal("engine domain whose edges acknowledged its certificate is still due")
	}
	// A failed projection of the same certificate is restored to ready by the completion check, which
	// accepts the engine edges' acknowledgements as the current edge coverage.
	f.exec(t, `UPDATE tunnel_domains SET certificate_state='failed' WHERE id=$1`, domain)
	restored, err := f.database.Queries().RecoverTunnelDomainCertificateProjectionV1(f.ctx, dbsqlc.RecoverTunnelDomainCertificateProjectionV1Params{Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}, DomainID: domain, AccountID: f.account, ExpectedGeneration: 1, CertificateID: certID, CertificateGeneration: 1, CertificateReference: certID})
	if err != nil || restored != 1 {
		t.Fatalf("engine domain certificate completion = %d, %v; want the projection restored", restored, err)
	}
	// An address change is make before break: the old host stays the route's name and the new one rides along as a
	// second name of the same route on every edge until each edge has observed it, then the new host is the name
	// and the old one is retired. A move the edges do not confirm in time is undone.
	repo, err = tunnelv1.NewRepository(f.database)
	if err != nil {
		t.Fatal(err)
	}
	policy := tunnelv1.AddressPolicy{MoveTimeout: 5 * time.Minute, Cooldown: time.Hour}
	oldHost := endpointID + ".tunnels.example.test"
	label := "renamed-" + endpointID[:8]
	renamed := label + ".tunnels.example.test"
	currentGeneration := func() int64 {
		var g int64
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT generation FROM tunnels WHERE id=$1`, tunnel).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	patch := func(name string) tunnelv1.MutationRecord {
		t.Helper()
		key := "addr-" + name + "-" + f.prefix
		out, err := repo.Patch(f.ctx, tunnelv1.PatchRecord{
			OperationID: "op_" + key, AuditEventID: "aud_" + key, TunnelID: tunnel, AccountID: f.account, Subdomain: &name, AddressPolicy: policy,
			ExpectedGeneration: currentGeneration(), IdempotencyKey: key, RequestHash: sha256.Sum256([]byte(key)),
			ActorID: f.account, AuditActorID: f.account, ActorType: "user", RequestID: "req_" + key, CorrelationID: "corr_" + key, Now: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	settle := func(at time.Time) {
		t.Helper()
		if err := repo.SettleAddressMoves(f.ctx, tunnelv1.AddressSettleRecord{Now: at, Limit: 10, Policy: policy, ActorID: "settler", ActorType: "system", RequestID: "req_settle", CorrelationID: "corr_settle"}); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := func() string {
		var e string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT stable_endpoint FROM tunnels WHERE id=$1`, tunnel).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	holds := func() string {
		var h string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT coalesce(string_agg(kind||':'||hostname,',' ORDER BY hostname),'') FROM tunnel_address_holds WHERE tunnel_id=$1`, tunnel).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	allReady := func() {
		f.exec(t, `UPDATE endpoint_edge_assignments SET observed_ready=true,observed_at=now() WHERE endpoint_id=$1 AND desired='active'`, tunnel)
	}
	var before int64
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT revision FROM endpoint_routes WHERE id=$1`, route).Scan(&before); err != nil {
		t.Fatal(err)
	}
	allReady()
	started := patch(label)
	if started.Tunnel.StableEndpoint != "https://"+oldHost || holds() != "incoming:"+renamed {
		t.Fatalf("after the change started: address %q holds %q; want the old address and the new name held", started.Tunnel.StableEndpoint, holds())
	}
	rows = snapshot()
	if len(rows) != 1 || rows[0].PublicHost != oldHost || !strings.Contains(rows[0].DomainBindings, renamed) || rows[0].ObservedState != "pending" || rows[0].RouteGeneration != before+1 {
		t.Fatalf("edge snapshot while the address moves = %+v, want the old host with %q as a second name", rows, renamed)
	}
	settle(time.Now().UTC())
	if endpoint() != "https://"+oldHost || holds() != "incoming:"+renamed {
		t.Fatalf("a move no edge confirmed yet settled: address %q holds %q", endpoint(), holds())
	}
	allReady()
	settle(time.Now().UTC())
	if endpoint() != "https://"+renamed || holds() != "retired:"+oldHost {
		t.Fatalf("after the edges confirmed: address %q holds %q; want the new address and the old name held", endpoint(), holds())
	}
	var mirrored string
	var revision int64
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT hostname,revision FROM endpoint_routes WHERE id=$1`, route).Scan(&mirrored, &revision); err != nil || mirrored != renamed || revision <= before+1 {
		t.Fatalf("engine route after the address change = %q rev %d, %v; want %q", mirrored, revision, err, renamed)
	}
	if rows := snapshot(); len(rows) != 1 || rows[0].PublicHost != renamed || strings.Contains(rows[0].DomainBindings, oldHost) {
		t.Fatalf("edge snapshot after the address change = %+v, want one route at %q", rows, renamed)
	}
	var phase string
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE resource_id=$1 AND event_type IN ('tunnel.address_change_started','tunnel.address_changed')`, tunnel).Scan(&phase); err != nil || phase != "2" {
		t.Fatalf("address audit events = %q, %v; want started and changed", phase, err)
	}
	// A move the edges never confirm is undone at its deadline: the old address keeps serving, the new name is
	// dropped and the operation fails.
	second := patch("never-" + endpointID[:8])
	if holds() != "retired:"+oldHost+",incoming:never-"+endpointID[:8]+".tunnels.example.test" || second.Tunnel.StableEndpoint != "https://"+renamed {
		t.Fatalf("second move: address %q holds %q", second.Tunnel.StableEndpoint, holds())
	}
	settle(time.Now().UTC().Add(policy.MoveTimeout + time.Minute))
	var opState string
	if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT state||'/'||coalesce(error_code,'') FROM operations WHERE id=$1`, "op_addr-never-"+endpointID[:8]+"-"+f.prefix).Scan(&opState); err != nil {
		t.Fatal(err)
	}
	if endpoint() != "https://"+renamed || holds() != "retired:"+oldHost || opState != "failed/tunnel_address_not_ready" {
		t.Fatalf("undone move: address %q holds %q operation %q", endpoint(), holds(), opState)
	}
	if rows := snapshot(); len(rows) != 1 || rows[0].PublicHost != renamed || strings.Contains(rows[0].DomainBindings, "never-") {
		t.Fatalf("edge snapshot after the undone move = %+v", rows)
	}
	// A tunnel paused (or deleted) during a move has no edge to confirm the name: the move is undone at once and its
	// operation fails, instead of waiting out the timeout (or hanging).
	for _, state := range []string{"paused", "deleted"} {
		f.exec(t, `UPDATE tunnels SET desired_state='active',deleted_at=NULL WHERE id=$1`, tunnel)
		allReady()
		label := state + "-" + endpointID[:8]
		patch(label)
		if h := holds(); !strings.Contains(h, "incoming:"+label) {
			t.Fatalf("%s: holds %q", state, h)
		}
		if state == "paused" {
			f.exec(t, `UPDATE tunnels SET desired_state='paused' WHERE id=$1`, tunnel)
		} else {
			f.exec(t, `UPDATE tunnels SET desired_state='deleted',deleted_at=now() WHERE id=$1`, tunnel)
		}
		settle(time.Now().UTC())
		var opState string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT state FROM operations WHERE id=$1`, "op_addr-"+label+"-"+f.prefix).Scan(&opState); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(holds(), "incoming") || opState != "failed" || endpoint() != "https://"+renamed {
			t.Fatalf("%s during a move: holds %q operation %q address %q", state, holds(), opState, endpoint())
		}
	}
	f.exec(t, `UPDATE tunnels SET desired_state='active',deleted_at=NULL WHERE id=$1`, tunnel)
	sync()
	attach([]string{runtimewire.CapabilityEndpoints})
	if endpoint, _, _ := state(); endpoint {
		t.Fatal("tunnel stayed on the engine after its agent stopped serving tunnels")
	}
}

// engineRouteGolden is the engine route row the server sends an edge, kept where the edge's test
// reads it (edge/internal/runtime: TestEdgeAdmitsTheServersEngineRouteGolden), so both sides run
// their real code against one contract. PAPERBOAT_UPDATE_GOLDEN=<that testdata directory> rewrites it.
const engineRouteGolden = "../../../shared/runtimeattachment/testdata/engine_route_snapshot.json"

func checkEngineRouteGolden(t *testing.T, item map[string]any) {
	t.Helper()
	encoded, err := json.MarshalIndent(map[string]any{"complete": true, "routes": []any{item}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("PAPERBOAT_UPDATE_GOLDEN"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(engineRouteGolden)), append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(engineRouteGolden)
	if err != nil {
		t.Fatalf("read the engine route golden (PAPERBOAT_UPDATE_GOLDEN=<testdata dir> writes it): %v", err)
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
