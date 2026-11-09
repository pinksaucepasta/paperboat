package browserterminal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// hostSnapshotAuthorizer is the real decision source (usermachines on PostgreSQL); only the host's own answer of
// its process generation, which needs a machine, is the fixture's.
type hostSnapshotAuthorizer struct{ MachineAuthorizer }

func (hostSnapshotAuthorizer) SnapshotProcess(context.Context, Decision) (int64, error) {
	return 1, nil
}

// pbvm1 2026-11-05 (vm1-1a6479a4-124638): pb update's new worker attached its carriers, the edge that observed one
// first became the route's active edge, and the control channel's 2 s recheck ended the browser terminal held on the
// other edge as "revoked" (3.8 s pause). Here on PostgreSQL, with the real runtime carrier attachment, route
// activation and browser terminal decision: the route's applied edge moves while the terminal is attached on the
// first edge; the terminal stays through two rechecks; revoking the machine still ends it within one.
func TestHeldBrowserTerminalSurvivesItsRouteMovingEdgesPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	prefix := "btmove_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	account, env, machine, route, session := prefix+"_account", prefix+"_env", prefix+"_machine", prefix+"_route", prefix+"_terminal"
	nodes := []string{prefix + "_edge_a", prefix + "_edge_b"}
	epochs := map[string]string{nodes[0]: "epoch_edge_a", nodes[1]: "epoch_edge_b"}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, step := range []struct{ query, id string }{
			{`DELETE FROM user_machine_terminal_sessions WHERE id=$1`, session}, {`DELETE FROM runtime_carrier_attachments WHERE machine_id=$1`, machine},
			{`DELETE FROM control_routes WHERE id=$1`, route}, {`DELETE FROM control_connector_generations WHERE environment_id=$1`, env},
			{`DELETE FROM user_machines WHERE id=$1`, machine}, {`DELETE FROM control_environments WHERE id=$1`, env},
			{`DELETE FROM control_tunnel_nodes WHERE id=$1`, nodes[0]}, {`DELETE FROM control_tunnel_nodes WHERE id=$1`, nodes[1]},
			{`DELETE FROM users WHERE id=$1`, account},
		} {
			_, _ = database.SQL().ExecContext(cleanup, step.query, step.id)
		}
		database.Close()
	})
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host := strings.ReplaceAll(machine, "_", "-") + ".runtime.example.test"
	exec(`INSERT INTO users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,$2,true,'active')`, account, account+"@example.test")
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, account)
	exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation,worker_generation,configured_capabilities,observed_capabilities) VALUES ($1,$2,$3,'edge-move','linux','amd64','/workspace','online','occupied',true,$4,1,1,ARRAY['terminal_host']::text[],ARRAY['terminal_host']::text[])`, machine, account, env, base64.RawURLEncoding.EncodeToString(public))
	exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,managed_public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,$3,'127.0.0.1',38080)`, route, env, host)
	exec(`INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
	for i, node := range nodes {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,endpoint_host,public_ingress_verified_at)
VALUES($1,'default','1.0',$2,'ready',true,now(),$3,$3,ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour',$4,27443,27444,$5,'fixture certificate',$4,now())`,
			node, epochs[node], "region-"+string(rune('a'+i)), "edge-"+string(rune('a'+i))+".example.test", "sha256:"+strings.Repeat(string(rune('1'+i)), 64))
	}
	exec(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,desired_state,runtime_state) VALUES($1,$2,$3,'held','/workspace','open','running')`, session, machine, account)

	// The worker attaches its carriers; both edges observe them; the route is applied to the active one.
	carriers := &runtimeattachment.Service{DB: database}
	proof := previewattachment.MachineProof{UserID: account, MachineID: machine, OperationID: "operation_edge_move", InstallationGeneration: 1}
	attach := func(request runtimewire.Request) map[string]runtimewire.Admission {
		t.Helper()
		answer, err := carriers.Attach(ctx, proof, request)
		if err != nil {
			t.Fatal(err)
		}
		byNode := map[string]runtimewire.Admission{answer.Admission.Binding.EdgeNodeID: answer.Admission}
		if answer.Backup != nil {
			byNode[answer.Backup.Binding.EdgeNodeID] = *answer.Backup
		}
		return byNode
	}
	observe := func(node string, carrier runtimewire.Admission) {
		t.Helper()
		edge := previewattachment.PreviewEdgeIdentity{NodeID: node, ProcessEpoch: epochs[node]}
		if err := carriers.Observe(ctx, edge, []runtimewire.Observation{{RouteID: route, SessionID: carrier.Binding.SessionID, AttachmentGeneration: carrier.AttachmentGeneration, Ready: true}}); err != nil {
			t.Fatal(err)
		}
	}
	applied := func() string {
		t.Helper()
		var node *string
		if err := database.SQL().QueryRowContext(ctx, `SELECT applied_node_id FROM control_routes WHERE id=$1`, route).Scan(&node); err != nil {
			t.Fatal(err)
		}
		if node == nil {
			return ""
		}
		return *node
	}
	first := runtimewire.Request{OperationID: proof.OperationID, ProcessID: "process_first", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 48080}
	old := attach(first)
	if len(old) != 2 {
		t.Fatalf("the worker got carriers to %d edges, want both", len(old))
	}
	for node, carrier := range old {
		observe(node, carrier)
	}
	attach(first)
	held := applied()
	moved := nodes[0]
	if held == nodes[0] {
		moved = nodes[1]
	}
	if held == "" {
		t.Fatal("the route was never applied to an edge")
	}

	authorizer := hostSnapshotAuthorizer{MachineAuthorizer{Service: usermachines.New(database, nil, usermachines.Policy{}, nil)}}
	decision, err := authorizer.ResolveRole(ctx, account, session)
	if err != nil || decision.EdgeNodeID != held || decision.HostWSSURL == "" {
		t.Fatalf("owner decision before the update = %+v, %v; want the route's edge %s", decision, err, held)
	}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: account, BrowserSessionID: "ses_edge_move", TerminalSessionID: session, Origin: "https://dashboard.example.test", Role: decision.Role, PolicyGeneration: decision.PolicyGeneration, TeamGeneration: decision.TeamGeneration, MembershipGeneration: decision.MembershipGeneration, BindingGeneration: decision.BindingGeneration, BrowserPublicKeySHA256: testBrowserKeyDigest}
	tickets := &edgeTestTickets{active: true, ticket: ticket, value: "edge-move-ticket"}
	gateway := &Gateway{Tickets: tickets, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.ServeEdgeControl(w, r, held, epochs[held])
	}))
	defer server.Close()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	wss, err := url.Parse(decision.HostWSSURL)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(EdgeControlRequest{Ticket: tickets.value, Origin: ticket.Origin, Host: wss.Host})
	if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	if kind, _, err := connection.Read(ctx); err != nil || kind != websocket.MessageText {
		t.Fatalf("the terminal was not admitted on edge %s: %v", held, err)
	}
	heartbeat := func(what string) {
		t.Helper()
		readCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		if kind, message, err := connection.Read(readCtx); err != nil || kind != websocket.MessageText || string(message) != "ok" {
			t.Fatalf("%s: the held terminal ended: %v", what, err)
		}
	}
	heartbeat("attached")

	// pb update: the next worker generation registers and attaches new carriers on both edges; only the other
	// edge has observed its carrier when the worker attaches again, so that edge becomes the route's active one.
	exec(`UPDATE user_machines SET worker_generation=2 WHERE id=$1`, machine)
	next := first
	next.ProcessID, next.WorkerGeneration = "process_next", 2
	fresh := attach(next)
	observe(moved, fresh[moved])
	attach(next)
	if got := applied(); got != moved {
		t.Fatalf("the route's applied edge is %q after the update, want it moved to %s (the fixture no longer reproduces the device)", got, moved)
	}
	heartbeat("the route moved to another edge (1st recheck)")
	heartbeat("the route moved to another edge (2nd recheck)")

	// Revocation still wins: the next recheck ends the terminal.
	exec(`UPDATE user_machines SET revoked_at=now() WHERE id=$1`, machine)
	readCtx, done := context.WithTimeout(ctx, 3*time.Second)
	defer done()
	for {
		kind, message, err := connection.Read(readCtx)
		if err == nil && kind == websocket.MessageText && string(message) == "ok" {
			continue // a heartbeat already on its way when the machine was revoked
		}
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("a revoked machine's held terminal: %v, want a policy close within one recheck", err)
		}
		break
	}
}
