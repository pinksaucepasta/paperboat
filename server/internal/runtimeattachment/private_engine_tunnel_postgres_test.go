package runtimeattachment

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/privateaccess"
)

// PLAN 3a step 4.4: a private tunnel's HTTP route rides the endpoint engine, so private access
// discovers it on the agent's runtime carrier (its connector and session) and the resolver
// authorizes exactly that carrier; nothing reads the connector tables.
func TestPrivateEngineTunnelRouteIsDiscoveredAndResolvedOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "privengine")
	tunnel, route := "tunnel_"+f.prefix, "route_"+f.prefix
	endpointID := uuid.NewString()
	host := endpointID + ".tunnels.example.test"
	f.exec(t, `UPDATE user_machines SET online=true WHERE id=$1`, f.machine)
	f.exec(t, `INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','private',1,$3,$4,$5,$2)`,
		tunnel, f.account, "endpoint_"+endpointID, "https://"+host, f.machine)
	f.exec(t, `INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,match_hostname,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web',1,'http','managed',$4,'http','127.0.0.1:3000','not_applicable',3,'active',$3,$3,now(),now())`, route, tunnel, f.account, host)
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
	if err := endpoints.SyncTunnelAgents(f.ctx, f.database, 100); err != nil {
		t.Fatal(err)
	}
	control := controlplane.NewEdgeService(f.database, "")
	rows, err := control.ListTunnelEdgeRouteAssignmentsForNodeV1(f.ctx, f.node, f.edge.ProcessEpoch)
	if err != nil || len(rows) != 1 || rows[0].AccessMode != "private" {
		t.Fatalf("private engine route on the edge: %+v %v", rows, err)
	}
	row := rows[0]
	if err := control.ObserveRoutes(f.ctx, f.node, []controlplane.RouteObservation{{
		RouteID: route, AssignmentID: row.AssignmentID, AssignmentGeneration: row.AssignmentGeneration, RouteRevision: row.RouteRevision,
		EdgeNodeID: f.node, EdgeProcessEpoch: f.edge.ProcessEpoch, ConnectorID: row.ConnectorID, HostID: row.HostID,
		ConnectorGeneration: 1, ConnectorSessionID: row.ConnectorSessionID, ConnectorProcessGeneration: row.ConnectorProcessGeneration,
		ConfigGeneration: row.ConfigGeneration, ConfigContentHash: "sha256:" + hex.EncodeToString(row.ConfigContentHash),
		AccessMode: row.AccessMode, ViewerPolicyGeneration: row.ViewerPolicyGeneration, ObservedState: "ready",
	}}); err != nil {
		t.Fatal(err)
	}
	repository, err := privateaccess.NewAccessorRepository(f.database)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := repository.MachineRoutes(f.ctx, f.account, f.machine)
	if err != nil || len(routes) != 1 {
		t.Fatalf("private access discovery = %+v, %v; want the engine route on the one ready edge", routes, err)
	}
	r := routes[0]
	if r.ResourceID != tunnel || r.RouteID != route || r.Protocol != "http" || r.Hostname != host || r.ConnectorID != row.ConnectorID || r.CarrierSessionID != row.ConnectorSessionID || r.EdgeNodeID != f.node || r.AssignmentID != row.AssignmentID {
		t.Fatalf("discovered route = %+v, want the agent carrier of %+v", r, row)
	}
	if admissions, err := repository.EdgeAdmissions(f.ctx, f.node, f.edge.ProcessEpoch); err != nil || len(admissions) != 1 || admissions[0].RouteID != route {
		t.Fatalf("edge admissions = %+v, %v", admissions, err)
	}
	resolver, err := privateaccess.NewSQLResolver(f.database, noAttachments{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	q := privateaccess.Request{AccountID: f.account, ResourceKind: privateaccess.ResourceTunnel, ResourceID: tunnel, RouteID: route, Audience: privateaccess.AudienceTunnelHTTP,
		MachineID: f.machine, SessionID: "installation_1", InstallationGeneration: 1, ExpiresAt: now.Add(30 * time.Second), Nonce: "nonce_1",
		ConnectorID: r.ConnectorID, CarrierSessionID: r.CarrierSessionID, RouteGeneration: r.RouteGeneration, SessionGeneration: r.SessionGeneration,
		ProcessGeneration: r.ProcessGeneration, ConfigGeneration: r.ConfigGeneration, AssignmentGeneration: r.AssignmentGeneration,
		EdgeNodeID: f.node, EdgeProcessEpoch: f.edge.ProcessEpoch, Protocol: privateaccess.ProtocolHTTP, Method: "GET", Host: host, Path: "/",
		IdempotencyKey: "operation_issue_1", RequestID: "req_001", CorrelationID: "corr_001"}
	binding, err := resolver.ResolvePrivate(f.ctx, privateaccess.Lookup{AccountID: f.account, Request: q, Edge: privateaccess.EdgeIdentity{NodeID: f.node, ProcessEpoch: f.edge.ProcessEpoch}, Now: now})
	if err != nil || binding.ConnectorID != r.ConnectorID || binding.CarrierSessionID != r.CarrierSessionID || binding.Protocol != privateaccess.ProtocolHTTP || binding.AssignmentGeneration != r.AssignmentGeneration {
		t.Fatalf("resolved binding = %+v, %v", binding, err)
	}
}

type noAttachments struct{}

func (noAttachments) Get(context.Context, string, string) (previewattachment.Attachment, error) {
	return previewattachment.Attachment{}, privateaccess.ErrResourceNotFound
}
