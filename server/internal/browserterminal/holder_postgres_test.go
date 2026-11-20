package browserterminal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// holderFixture is a machine whose route has valid carriers on two edge nodes (a and b), a third node that holds
// none (c), the real decision source and the real ticket store, on PostgreSQL.
type holderFixture struct {
	gateway          *Gateway
	account, session string
	browserSession   string
	node, epoch      map[string]string
	host             string
	origin           string
	resumed          []string
	issue            func() (Ticket, string)
}

func newHolderFixture(t *testing.T) *holderFixture {
	t.Helper()
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
	t.Cleanup(cancel)
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	prefix := "bthold_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	f := &holderFixture{account: prefix + "_account", session: prefix + "_terminal", browserSession: prefix + "_ses", origin: "https://dashboard.example.test", node: map[string]string{}, epoch: map[string]string{}}
	env, machine, route := prefix+"_env", prefix+"_machine", prefix+"_route"
	names := []string{"a", "b", "c"}
	for _, n := range names {
		f.node[n], f.epoch[n] = prefix+"_edge_"+n, "epoch_"+n
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		steps := []struct{ query, id string }{
			{`DELETE FROM browser_terminal_tickets WHERE browser_session_id=$1`, f.browserSession}, {`DELETE FROM sessions WHERE id=$1`, f.browserSession},
			{`DELETE FROM user_machine_terminal_sessions WHERE id=$1`, f.session}, {`DELETE FROM runtime_carrier_attachments WHERE machine_id=$1`, machine},
			{`DELETE FROM control_routes WHERE id=$1`, route}, {`DELETE FROM control_connector_generations WHERE environment_id=$1`, env},
			{`DELETE FROM user_machines WHERE id=$1`, machine}, {`DELETE FROM control_environments WHERE id=$1`, env},
		}
		for _, n := range names {
			steps = append(steps, struct{ query, id string }{`DELETE FROM control_tunnel_nodes WHERE id=$1`, f.node[n]})
		}
		steps = append(steps, struct{ query, id string }{`DELETE FROM users WHERE id=$1`, f.account})
		for _, step := range steps {
			_, _ = database.SQL().ExecContext(cleanup, step.query, step.id)
		}
		database.Close()
	})
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.host = strings.ReplaceAll(machine, "_", "-") + ".runtime.example.test"
	exec(`INSERT INTO users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,$2,true,'active')`, f.account, f.account+"@example.test")
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, f.account)
	exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation,worker_generation,configured_capabilities,observed_capabilities) VALUES ($1,$2,$3,'holder','linux','amd64','/workspace','online','occupied',true,$4,1,1,ARRAY['terminal_host']::text[],ARRAY['terminal_host']::text[])`, machine, f.account, env, base64.RawURLEncoding.EncodeToString(public))
	exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,managed_public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,$3,'127.0.0.1',38080)`, route, env, f.host)
	exec(`INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
	for i, n := range names[:2] {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,endpoint_host,public_ingress_verified_at)
VALUES($1,'default','1.0',$2,'ready',true,now(),$3,$3,ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour',$4,27443,27444,$5,'fixture certificate',$4,now())`,
			f.node[n], f.epoch[n], "region-"+n, "edge-"+n+".example.test", "sha256:"+strings.Repeat(string(rune('1'+i)), 64))
	}
	exec(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,desired_state,runtime_state) VALUES($1,$2,$3,'held','/workspace','open','running')`, f.session, machine, f.account)

	// The worker attaches; the edges a and b observe their carriers ready (node c never gets one).
	carriers := &runtimeattachment.Service{DB: database}
	proof := previewattachment.MachineProof{UserID: f.account, MachineID: machine, OperationID: "operation_holder", InstallationGeneration: 1}
	request := runtimewire.Request{OperationID: proof.OperationID, ProcessID: "process_holder", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 48080}
	held := map[string]bool{}
	for pass := range 2 {
		answer, err := carriers.Attach(ctx, proof, request)
		if err != nil {
			t.Fatal(err)
		}
		admitted := []runtimewire.Admission{answer.Admission}
		if answer.Backup != nil {
			admitted = append(admitted, *answer.Backup)
		}
		// The second attach is the one that applies the route to the edge that observed first.
		for _, carrier := range admitted {
			if pass == 1 {
				break
			}
			node := carrier.Binding.EdgeNodeID
			edge := previewattachment.PreviewEdgeIdentity{NodeID: node, ProcessEpoch: carrier.Binding.EdgeProcessEpoch}
			if err := carriers.Observe(ctx, edge, []runtimewire.Observation{{RouteID: route, SessionID: carrier.Binding.SessionID, AttachmentGeneration: carrier.AttachmentGeneration, Ready: true}}); err != nil {
				t.Fatal(err)
			}
			held[node] = true
		}
	}
	// Node c joins only now, so it cannot be given a carrier; the browser session is only needed to issue tickets.
	for i, n := range names[2:] {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,endpoint_host,public_ingress_verified_at)
VALUES($1,'default','1.0',$2,'ready',true,now(),$3,$3,ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour',$4,27443,27444,$5,'fixture certificate',$4,now())`,
			f.node[n], f.epoch[n], "region-"+n, "edge-"+n+".example.test", "sha256:"+strings.Repeat(string(rune('3'+i)), 64))
	}
	exec(`INSERT INTO sessions (id,user_id,session_hash,csrf_hash,expires_at) VALUES ($1,$2,$3,$4,now()+interval '1 hour')`, f.browserSession, f.account, "hash_"+prefix, "csrf_"+prefix)
	if !held[f.node["a"]] || !held[f.node["b"]] || held[f.node["c"]] {
		t.Fatalf("carriers are on %v, want exactly edges a and b (the fixture no longer places them)", held)
	}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.gateway = &Gateway{
		Tickets:           TicketStore{DB: database},
		Authorizer:        hostSnapshotAuthorizer{MachineAuthorizer{Service: usermachines.New(database, nil, usermachines.Policy{}, nil)}},
		HoldsCarrier:      CarrierHolder{DB: database}.Holds,
		Signer:            signer,
		Issuer:            "https://api.example.test",
		AllowedOrigins:    []string{f.origin},
		AdmissionLifetime: time.Minute,
		RecordUsageResumed: func(_ context.Context, attachment, _, node string, _, _ time.Time) error {
			f.resumed = append(f.resumed, node+"/"+attachment)
			return nil
		},
	}
	f.issue = func() (Ticket, string) {
		t.Helper()
		ticket, streamURL, _, err := f.gateway.Issue(ctx, f.account, f.browserSession, f.session, f.origin, testBrowserKeyDigest)
		if err != nil {
			t.Fatal(err)
		}
		return ticket, streamURL
	}
	return f
}

func (f *holderFixture) request(t *testing.T, streamURL, ticket string) EdgeControlRequest {
	t.Helper()
	u, err := url.Parse(streamURL)
	if err != nil || u.Host != f.host || u.Path != "/v1/browser-terminal" {
		t.Fatalf("stream URL %q (%v), want wss://%s/v1/browser-terminal", streamURL, err, f.host)
	}
	return EdgeControlRequest{Ticket: ticket, Origin: f.origin, Host: u.Host}
}

func (f *holderFixture) admit(t *testing.T, req EdgeControlRequest, n string) (edgeAdmission, error) {
	t.Helper()
	admission, release, err := f.gateway.admitEdge(context.Background(), req, f.node[n], f.epoch[n])
	if release != nil {
		release()
	}
	return admission, err
}

// A ticket names the runtime hostname, so whichever edge process holds a valid carrier for the machine's route
// redeems it, not only the one the route was applied to when it was issued.
func TestTicketRedeemedByAnyCarrierHolder(t *testing.T) {
	f := newHolderFixture(t)
	for _, n := range []string{"a", "b"} {
		ticket, streamURL := f.issue()
		admission, err := f.admit(t, f.request(t, streamURL, ticket.Value), n)
		if err != nil || admission.Credential == "" || admission.AttachmentID == "" {
			t.Fatalf("edge %s holds a valid carrier but was refused: %+v, %v", n, admission, err)
		}
	}
}

// A node without a valid carrier for the route never redeems the ticket (it forwards the stream instead), and a
// refused redemption does not leave a usable ticket behind.
func TestTicketRefusedByNodeWithoutCarrier(t *testing.T) {
	f := newHolderFixture(t)
	ticket, streamURL := f.issue()
	req := f.request(t, streamURL, ticket.Value)
	if _, err := f.admit(t, req, "c"); err == nil {
		t.Fatal("an edge node without a carrier redeemed the ticket")
	}
	if _, err := f.admit(t, req, "a"); err == nil {
		t.Fatal("the ticket stayed usable after a refused redemption")
	}
	// The same node name with another process epoch is not the process that holds the carrier either.
	other, otherURL := f.issue()
	if _, _, err := f.gateway.admitEdge(context.Background(), f.request(t, otherURL, other.Value), f.node["a"], "epoch_stale"); err == nil {
		t.Fatal("a stale process epoch redeemed the ticket")
	}
}

// A ticket is one use across nodes: redeemed at one holder it is spent at every other.
func TestTicketStillSingleUseAcrossNodes(t *testing.T) {
	f := newHolderFixture(t)
	ticket, streamURL := f.issue()
	req := f.request(t, streamURL, ticket.Value)
	if _, err := f.admit(t, req, "a"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if _, err := f.admit(t, req, n); err == nil {
			t.Fatalf("a redeemed ticket was redeemed again at edge %s", n)
		}
	}
}

// An attachment admitted through one holder resumes through a holder (the usage row still names the node it was
// admitted at); a node without a carrier is refused.
func TestResumeAdmittedFromAnotherCarrierHolder(t *testing.T) {
	f := newHolderFixture(t)
	ticket, streamURL := f.issue()
	req := f.request(t, streamURL, ticket.Value)
	admitted, err := f.admit(t, req, "a")
	if err != nil {
		t.Fatal(err)
	}
	req.Resume = admitted.AttachmentID
	resume := func(n string) error {
		admission, release, err := f.gateway.resumeEdge(context.Background(), req, f.node[n], f.epoch[n])
		if release != nil {
			release()
		}
		if err == nil && admission.AttachmentID != admitted.AttachmentID {
			t.Fatalf("resumed attachment %q, want %q", admission.AttachmentID, admitted.AttachmentID)
		}
		return err
	}
	if err := resume("b"); err != nil {
		t.Fatalf("a carrier holder was refused the resume: %v", err)
	}
	if got := f.resumed; len(got) != 1 || got[0] != f.node["b"]+"/"+admitted.AttachmentID {
		t.Fatalf("usage resumed as %v, want it to name the resuming node b", got)
	}
	err = resume("c")
	var answer refusal
	if !errors.As(err, &answer) {
		t.Fatalf("a node without a carrier resumed: %v", err)
	}
}
