package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/inspectaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestInspectorRemoteAccessPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	suffix := fmt.Sprint(now.UnixNano())
	owner, mate, lease, tunnel, route, node := inspectorHTTPFixture(t, ctx, database, now, suffix)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := database.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET registry_expires_at=$1,region='test',failure_domain='test',roles=ARRAY['edge'],transports=ARRAY['http3','http2'],capacity_limit=10,capacity_observed_at=now() WHERE id=$2`, now.Add(time.Hour), node)
	cli := "cli_ir_" + suffix
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$3,'inspector remote','desktop','test',ARRAY['machines:connect'],'active',$4,$4)`, cli, mate, "client_ir_"+suffix, now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.cli_client_sessions WHERE id=$1`, cli)
	})
	service := inspectaccess.NewService(database)
	// mate may also inspect the tunnel (checked below). A policy save fences inspector
	// credentials issued before it, so the access is set before mate's preview credential.
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'tunnel',$2,$3,true,1)`, "team_ih_"+suffix, tunnel, owner)
	teamstest.SetResourceAccess(t, database, "team_ih_"+suffix, mate, "tunnel", tunnel, "inspect", true)
	grant, err := service.Issue(ctx, inspectaccess.IssueRequest{AccountID: mate, ResourceKind: "preview", ResourceID: lease, RouteID: lease, Action: "inspect", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	input := inspectaccess.AccessRequest{AccountID: mate, CLIClientSessionID: cli, Token: grant.Token, ResourceKind: "preview", ResourceID: lease, RouteID: lease, Action: "inspect", Transport: "native"}
	access, err := service.ResolveAccess(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if access.OwnerAccountID != owner || access.MachineID != "mch_ih_"+suffix {
		t.Fatalf("incorrect owner routing: %+v", access)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE id=$1`, node)
	if _, err = service.ResolveAccess(ctx, input); err != nil {
		t.Fatalf("native inspector depended on ready edge: %v", err)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=true WHERE id=$1`, node)
	var count int
	if err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.inspector_native_admissions WHERE credential_id=$1`, grant.CredentialID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("inspector network admission count=%d err=%v", count, err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE cli_client_session_id=$1`, cli).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected machine access count=%d err=%v", count, err)
	}

	exec(`UPDATE paperboat.tunnels SET created_by_host_id=$1 WHERE id=$2`, access.MachineID, tunnel)
	ownerCLI := "cli_ir_owner_" + suffix
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$3,'inspector owner','desktop','test',ARRAY['machines:connect'],'active',$4,$4)`, ownerCLI, owner, "client_ir_owner_"+suffix, now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.cli_client_sessions WHERE id=$1`, ownerCLI)
	})
	durableGrant, e := service.Issue(ctx, inspectaccess.IssueRequest{AccountID: owner, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "inspect", TTL: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	durable, e := service.ResolveAccess(ctx, inspectaccess.AccessRequest{AccountID: owner, CLIClientSessionID: ownerCLI, Token: durableGrant.Token, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "inspect", Transport: "native"})
	if e != nil || durable.MachineID != access.MachineID {
		t.Fatalf("durable native routing: %v", e)
	}

	connector, carrierSession, contentHash := insertEngineTunnelCarrier(t, database, owner, access.MachineID, tunnel, route, node, "epoch-ih-"+suffix, suffix)
	assignment := "epa_" + route
	edgeDurable, e := service.ResolveAccess(ctx, inspectaccess.AccessRequest{AccountID: owner, Token: durableGrant.Token, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "inspect", Transport: "edge", EdgeNodeID: node, EdgeProcessEpoch: "epoch-ih-" + suffix})
	if e != nil || edgeDurable.Carrier.AssignmentID != assignment || edgeDurable.Carrier.ConfigContentHash != contentHash || edgeDurable.Carrier.ConnectorID != connector || edgeDurable.Carrier.SessionID != carrierSession {
		t.Fatalf("durable edge routing: %v", e)
	}
	targets, e := service.Targets(ctx, mate, tunnel, "inspect")
	if e != nil || len(targets) != 1 || targets[0].ResourceID != tunnel || targets[0].RouteID != route {
		t.Fatalf("inspect-only target discovery: %v", e)
	}
	if _, e = service.Targets(ctx, mate, tunnel, "replay"); e == nil {
		t.Fatal("inspect-only target discovery admitted replay")
	}
	handlers := &InspectorHandlers{Access: service}
	body, _ := json.Marshal(map[string]any{"credential_token": grant.Token, "resource_kind": "preview", "resource_id": lease, "route_id": lease, "action": "inspect", "transport": "edge"})
	response := inspectorHTTPRequest(t, handlers.ResolveAccess, http.MethodPost, "/v1/inspector/access", string(body), mate)
	if response.Code != http.StatusOK {
		t.Fatalf("edge route status=%d", response.Code)
	}
	denied := input
	denied.Action = "replay"
	if _, err = service.ResolveAccess(ctx, denied); err == nil {
		t.Fatal("inspect credential admitted replay")
	}
	denied = input
	denied.Transport = "edge"
	denied.EdgeNodeID = "different-edge"
	denied.EdgeProcessEpoch = "different-epoch"
	if _, err = service.ResolveAccess(ctx, denied); err == nil {
		t.Fatal("different edge admitted")
	}

	exec(`UPDATE paperboat.tunnel_routes SET desired_state='deleted',deleted_at=now(),generation=generation+1 WHERE id=$1`, route)
	if _, e = service.Authorize(ctx, inspectaccess.AuthorizeRequest{MachineAccount: owner, Token: durableGrant.Token, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "inspect"}); e == nil {
		t.Fatal("deleted route remained authorized")
	}
	if _, e = service.Targets(ctx, mate, tunnel, "inspect"); e == nil {
		t.Fatal("deleted route remained discoverable")
	}
	teamstest.SetResourceAccess(t, database, "team_ih_"+suffix, mate, "preview", lease, "inspect", false)
	if _, err = service.ResolveAccess(ctx, input); err == nil {
		t.Fatal("revoked inspect grant admitted")
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.inspector_native_admissions WHERE credential_id=$1`, grant.CredentialID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("revoked network admission count=%d err=%v", count, err)
	}
}

// insertEngineTunnelCarrier serves the tunnel's route on the endpoint engine (PLAN 3a): an agent
// runtime carrier on the edge and a ready assignment of the route to it. It returns the carrier's
// connector, session and config content hash, the identity the edge names the route by.
func insertEngineTunnelCarrier(t *testing.T, database *db.DB, owner, machine, tunnel, route, node, epoch, suffix string) (connector, session, contentHash string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := database.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	connector = "con_engine_" + suffix
	session = fmt.Sprintf("session_00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1e12)
	contentHash = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(`{}`)))
	admission, _ := json.Marshal(map[string]any{"binding": map[string]any{"tunnel_id": "carrier_" + tunnel, "connector_id": connector, "config_generation": 1}, "config_content_hash": contentHash})
	carrierRoute := "runtime_engine_" + suffix
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) SELECT environment_id,environment_id,user_id,'active' FROM paperboat.user_machines WHERE id=$1 ON CONFLICT DO NOTHING`, machine)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) SELECT $1,environment_id,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080 FROM paperboat.user_machines WHERE id=$2`, carrierRoute, machine)
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active) SELECT $1,m.id,m.user_id,m.installation_generation,1,'process_engine',1,$2,$3,m.public_identity_key,1,$4,$4,$5::jsonb,now()+interval '1 hour',true FROM paperboat.user_machines m WHERE m.id=$6`, carrierRoute, node, epoch, session, string(admission), machine)
	exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'team','persistent',$1)`, tunnel, owner, machine)
	exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,'engine.tunnels.example.test','http',$1)`, route, tunnel)
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,$4,$5,true,now())`, route, tunnel, node, epoch, session)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.endpoints WHERE id=$1`, tunnel)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_routes WHERE id=$1`, carrierRoute)
	})
	return connector, session, contentHash
}
