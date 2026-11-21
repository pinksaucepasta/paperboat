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
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// Browser terminals always use Paperboat's managed edges at its own domain (user decision). On PostgreSQL with the
// real carrier attachment and terminal decision: a terminal is held on a managed edge, the workspace switches to
// its own servers (self-hosted-only), the terminal stays attached through the rechecks, and a new terminal of
// that workspace is admitted on the managed edge at the same hostname, never on the customer's server.
func TestHeldBrowserTerminalSurvivesAnEdgeModeSwitchPostgres(t *testing.T) {
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
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	prefix := "btmode_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	account, env, machine, route, session := prefix+"_account", prefix+"_env", prefix+"_machine", prefix+"_route", prefix+"_terminal"
	managed, customer, installation := prefix+"_edge_managed", prefix+"_edge_customer", prefix+"_installation"
	epochs := map[string]string{managed: "epoch_managed", customer: "epoch_customer"}
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
			{`DELETE FROM selfhost_pool_policies WHERE scope_id=$1`, account}, {`DELETE FROM workspace_edge_sets WHERE scope_id=$1`, account},
			{`DELETE FROM control_routes WHERE id=$1`, route}, {`DELETE FROM control_connector_generations WHERE environment_id=$1`, env},
			{`DELETE FROM user_machines WHERE id=$1`, machine}, {`DELETE FROM control_environments WHERE id=$1`, env},
			{`DELETE FROM selfhost_installations WHERE id=$1`, installation},
			{`DELETE FROM control_tunnel_nodes WHERE id=$1`, managed}, {`DELETE FROM control_tunnel_nodes WHERE id=$1`, customer},
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
	exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation,worker_generation,configured_capabilities,observed_capabilities) VALUES ($1,$2,$3,'edge-mode','linux','amd64','/workspace','online','occupied',true,$4,1,1,ARRAY['terminal_host']::text[],ARRAY['terminal_host']::text[])`, machine, account, env, base64.RawURLEncoding.EncodeToString(public))
	exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,managed_public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,$3,'127.0.0.1',38080)`, route, env, host)
	exec(`INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
	for i, node := range []string{managed, customer} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,endpoint_host,public_ingress_verified_at,tls_spki_sha256)
VALUES($1,'default','1.0',$2,'ready',true,now(),$3,$3,ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour',$4,7272,7273,$5,'fixture certificate',$4,now(),$6)`,
			node, epochs[node], "region-"+string(rune('a'+i)), "edge-"+string(rune('a'+i))+".example.test", "sha256:"+strings.Repeat(string(rune('1'+i)), 64), make([]byte, 32))
	}
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'fixture','own server','tunnel','account',$4,$1,true)`, installation, customer, []byte(public), account)
	exec(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,desired_state,runtime_state) VALUES($1,$2,$3,'held','/workspace','open','running')`, session, machine, account)

	// Managed-only: both edges are looked at, only the managed one is eligible; the terminal is held on it.
	carriers := &runtimeattachment.Service{DB: database}
	proof := previewattachment.MachineProof{UserID: account, MachineID: machine, OperationID: "operation_edge_mode", InstallationGeneration: 1}
	request := runtimewire.Request{OperationID: proof.OperationID, ProcessID: "process_mode", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 48080}
	attachAndObserve := func() runtimewire.HostAdmission {
		t.Helper()
		answer, err := carriers.Attach(ctx, proof, request)
		if err != nil {
			t.Fatal(err)
		}
		for _, carrier := range append([]runtimewire.Admission{answer.Admission}, func() []runtimewire.Admission {
			if answer.Backup != nil {
				return []runtimewire.Admission{*answer.Backup}
			}
			return nil
		}()...) {
			edge := previewattachment.PreviewEdgeIdentity{NodeID: carrier.Binding.EdgeNodeID, ProcessEpoch: epochs[carrier.Binding.EdgeNodeID]}
			if err := carriers.Observe(ctx, edge, []runtimewire.Observation{{RouteID: route, SessionID: carrier.Binding.SessionID, AttachmentGeneration: carrier.AttachmentGeneration, Ready: true}}); err != nil {
				t.Fatal(err)
			}
		}
		return answer
	}
	attachAndObserve()
	attachAndObserve()
	authorizer := hostSnapshotAuthorizer{MachineAuthorizer{Service: usermachines.New(database, nil, usermachines.Policy{}, nil)}}
	before, err := authorizer.ResolveRole(ctx, account, session)
	if err != nil || before.EdgeNodeID != managed || !strings.HasPrefix(before.HostWSSURL, "wss://"+host) {
		t.Fatalf("managed-only decision = %+v, %v; want the managed edge at %s", before, err, host)
	}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: account, BrowserSessionID: "ses_edge_mode", TerminalSessionID: session, Origin: "https://dashboard.example.test", Role: before.Role, PolicyGeneration: before.PolicyGeneration, TeamGeneration: before.TeamGeneration, MembershipGeneration: before.MembershipGeneration, BindingGeneration: before.BindingGeneration, BrowserPublicKeySHA256: testBrowserKeyDigest}
	tickets := &edgeTestTickets{active: true, ticket: ticket, value: "edge-mode-ticket"}
	gateway := &Gateway{Tickets: tickets, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}, HoldsCarrier: CarrierHolder{DB: database}.Holds}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.ServeEdgeControl(w, r, managed, epochs[managed])
	}))
	defer server.Close()
	admit := func(decision Decision) *websocket.Conn {
		t.Helper()
		connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		wss, err := url.Parse(decision.HostWSSURL)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(EdgeControlRequest{Ticket: tickets.value, Origin: ticket.Origin, Host: wss.Host})
		if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		if kind, _, err := connection.Read(ctx); err != nil || kind != websocket.MessageText {
			t.Fatalf("the terminal was not admitted on the managed edge: %v", err)
		}
		return connection
	}
	held := admit(before)
	defer held.CloseNow()
	heartbeat := func(connection *websocket.Conn, what string) {
		t.Helper()
		readCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		if kind, message, err := connection.Read(readCtx); err != nil || kind != websocket.MessageText || string(message) != "ok" {
			t.Fatalf("%s: the terminal ended: %v", what, err)
		}
	}
	heartbeat(held, "attached")

	// The workspace switches to its own servers: its machine re-attaches (own server for tunnels, the managed edge
	// still for terminals). The held terminal stays; its decision is unchanged.
	if _, err := selfhost.New(database, "edge-mode-test-key").SetPool(ctx, account, "tunnel", selfhost.Policy{Mode: "self-hosted-only", InstallationIDs: []string{installation}}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE control_tunnel_nodes SET last_heartbeat_at=now(),capacity_observed_at=now() WHERE id IN ($1,$2)`, managed, customer)
	answer := attachAndObserve()
	attachAndObserve()
	if answer.Binding.EdgeNodeID != managed || answer.Backup == nil || answer.Backup.Binding.EdgeNodeID != customer || answer.ActiveEdgeNodeID != managed {
		backup := ""
		if answer.Backup != nil {
			backup = answer.Backup.Binding.EdgeNodeID
		}
		t.Fatalf("self-hosted-only attach: primary=%s backup=%q active=%s, want the managed edge %s first and active and its own server %s second", answer.Binding.EdgeNodeID, backup, answer.ActiveEdgeNodeID, managed, customer)
	}
	after, err := authorizer.ResolveRole(ctx, account, session)
	if err != nil || after.EdgeNodeID != managed || after.HostWSSURL != before.HostWSSURL {
		t.Fatalf("self-hosted-only decision = %+v, %v; want the same managed edge and address %s", after, err, before.HostWSSURL)
	}
	heartbeat(held, "the workspace switched to its own servers (1st recheck)")
	heartbeat(held, "the workspace switched to its own servers (2nd recheck)")
	// A new terminal of the self-hosted-only workspace opens through the managed edge at Paperboat's hostname.
	tickets.mu.Lock()
	tickets.used = false // a ticket is redeemed once; this one stands for a second browser's
	tickets.mu.Unlock()
	fresh := admit(after)
	defer fresh.CloseNow()
	heartbeat(fresh, "a new terminal in the self-hosted-only workspace")
}
