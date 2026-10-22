package runtimeattachment

import (
	"context"
	"crypto/ed25519"
	"errors"
	"crypto/rand"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
)

// PLAN 3a step 1: a preview endpoint is served over the agent's runtime carriers. Its route is
// assigned to every edge where the agent holds a carrier that can serve endpoints, and is sent
// only to an edge that can admit it; the edge's observation marks the assignment ready.
func TestEndpointRoutesRideOnAgentRuntimeCarriersOnPostgres(t *testing.T) {
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
	defer database.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	prefix := "endpoint_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	account, machine, env, route, node, backupNode := prefix+"_account", prefix+"_machine", prefix+"_env", prefix+"_route", prefix+"_edge", prefix+"_zbackup"
	lease := prefix + "_preview"
	host := strings.ReplaceAll(prefix, "_", "-") + ".preview.example.test"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, query := range []string{`DELETE FROM endpoints WHERE agent_machine_id=$1`, `DELETE FROM preview_leases WHERE owner_machine_id=$1`, `DELETE FROM runtime_carrier_attachments WHERE machine_id=$1`} {
			_, _ = database.SQL().ExecContext(ctx, query, machine)
		}
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_routes WHERE id=$1`, route)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_connector_generations WHERE environment_id=$1`, env)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM user_machines WHERE id=$1`, machine)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_environments WHERE id=$1`, env)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_tunnel_nodes WHERE id IN ($1,$2)`, node, backupNode)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, account)
	}()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.RawURLEncoding.EncodeToString(public)
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, account)
	exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation,configured_capabilities) VALUES ($1,$2,$3,'endpoint-test','linux','amd64','/workspace','online','occupied',$4,1,1,ARRAY['terminal_host']::text[])`, machine, account, env, key)
	exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080)`, route, env, machine+".runtime.example.test")
	exec(`INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
	for _, n := range []struct{ id, epoch, region, domain, host, pin string }{
		{node, "epoch_endpoint", "first-region", "first-domain", "edge.example.test", "1"},
		{backupNode, "epoch_backup", "second-region", "second-domain", "backup.example.test", "2"},
	} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem)
VALUES($1,'default','1.0',$2,'ready',true,now(),$3,$4,ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour',$5,27443,27444,$6,'fixture certificate')`, n.id, n.epoch, n.region, n.domain, n.host, "sha256:"+strings.Repeat(n.pin, 64))
	}
	exec(`INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,$2,$3,$3,$4,'owner_session_endpoint','foreground','http','127.0.0.1:3000','team',now()+interval '10 minutes')`, lease, "https://"+host, account, machine)
	if err := database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: account, AgentMachineID: machine, AccessMode: "team", Endpoint: "https://" + host})
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{DB: database}
	proof := previewattachment.MachineProof{UserID: account, MachineID: machine, OperationID: "operation_endpoint", InstallationGeneration: 1}
	request := runtimewire.Request{OperationID: proof.OperationID, ProcessID: "process_endpoint", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 48080}
	edge := previewattachment.PreviewEdgeIdentity{NodeID: node, ProcessEpoch: "epoch_endpoint"}
	capable := []string{runtimewire.CapabilityEndpoints}
	assignments := func() map[string]string {
		t.Helper()
		rows, err := database.SQL().QueryContext(ctx, `SELECT edge_node_id,desired||'/'||observed_ready FROM endpoint_edge_assignments WHERE endpoint_id=$1`, lease)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var n, state string
			if err := rows.Scan(&n, &state); err != nil {
				t.Fatal(err)
			}
			out[n] = state
		}
		return out
	}

	oldPath := errors.New("old attachment path")
	readiness := endpoints.PreviewReadiness{DB: database, Attachments: attachmentReadinessFunc(func() error { return oldPath })}
	gate := func() error {
		return readiness.RequirePreviewAttachmentReady(ctx, account, lease, "operation_unused", machine, "owner_session_endpoint")
	}
	served := func() []string {
		t.Helper()
		rows, err := database.SQL().QueryContext(ctx, `SELECT edge_node_id||'/'||state FROM preview_routes WHERE preview_id=$1 AND endpoint ORDER BY 1`, lease)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}

	// An agent that does not advertise endpoints gets no assignment: its previews stay on the old path.
	if _, err := service.Attach(ctx, proof, request); err != nil {
		t.Fatal(err)
	}
	if got := assignments(); len(got) != 0 {
		t.Fatalf("assignments for an agent without the endpoints capability: %v", got)
	}
	if err := gate(); !errors.Is(err, oldPath) {
		t.Fatalf("readiness of a preview whose agent lacks the capability = %v, want the old attachment gate", err)
	}
	request.Capabilities = capable
	a, err := service.Attach(ctx, proof, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := assignments(); len(got) != 2 || got[node] != "active/false" || got[backupNode] != "active/false" {
		t.Fatalf("assignments after capable attach = %v, want both edges active and unobserved", got)
	}
	// No edge has advertised endpoints yet: the agent keeps its previews on the old path.
	if a.EndpointsServed {
		t.Fatal("endpoints served before any edge admitted them")
	}
	if err := gate(); !errors.Is(err, oldPath) {
		t.Fatalf("readiness before the edges admit endpoints = %v, want the old attachment gate", err)
	}
	backupEdge := previewattachment.PreviewEdgeIdentity{NodeID: backupNode, ProcessEpoch: "epoch_backup"}
	for _, e := range []previewattachment.PreviewEdgeIdentity{edge, backupEdge} {
		if _, err := service.Pull(ctx, e, capable); err != nil {
			t.Fatal(err)
		}
	}
	if a, err = service.Attach(ctx, proof, request); err != nil || !a.EndpointsServed {
		t.Fatalf("attachment after both edges admit endpoints: served=%v err=%v", a.EndpointsServed, err)
	}
	if err := gate(); !errors.Is(err, endpoints.ErrNotServing) {
		t.Fatalf("readiness before any edge observed the route = %v", err)
	}
	if got := served(); len(got) != 0 {
		t.Fatalf("unobserved route already served: %v", got)
	}
	// An edge that does not advertise endpoints gets only its runtime route.
	if values, err := service.Pull(ctx, edge, nil); err != nil || len(values) != 1 || values[0].RouteKind != runtimewire.RouteKind {
		t.Fatalf("pull from an edge without the endpoints capability = %+v, %v", values, err)
	}
	values, err := service.Pull(ctx, edge, capable)
	if err != nil || len(values) != 2 {
		t.Fatalf("capable pull = %d admissions, %v", len(values), err)
	}
	var endpointRoute runtimewire.Admission
	for _, v := range values {
		if v.EndpointID != "" {
			endpointRoute = v
		}
	}
	if endpointRoute.EndpointID != lease || endpointRoute.Hostname != host || endpointRoute.AccessMode != "team" || endpointRoute.RouteKind != runtimewire.EndpointRestrictedRouteKind {
		t.Fatalf("endpoint admission = %+v", endpointRoute)
	}
	// Same carrier identity as the runtime route, its own route id.
	if endpointRoute.Binding.SessionID != a.Binding.SessionID || endpointRoute.Binding.TunnelID != a.Binding.TunnelID || endpointRoute.Binding.RouteID != lease || endpointRoute.RouteRevision != endpointRoute.Binding.RouteGeneration {
		t.Fatalf("endpoint route does not ride on the runtime carrier: %+v vs %+v", endpointRoute.Binding, a.Binding)
	}
	if _, err := service.Pull(ctx, edge, []string{"something_else"}); err != ErrInvalid {
		t.Fatalf("unknown edge capability accepted: %v", err)
	}
	observation := runtimewire.Observation{RouteID: endpointRoute.Binding.RouteID, SessionID: endpointRoute.Binding.SessionID, AttachmentGeneration: endpointRoute.AttachmentGeneration, Ready: true}
	runtimeObservation := runtimewire.Observation{RouteID: route, SessionID: a.Binding.SessionID, AttachmentGeneration: a.AttachmentGeneration, Ready: true}
	if err := service.Observe(ctx, edge, []runtimewire.Observation{runtimeObservation, observation}); err != nil {
		t.Fatal(err)
	}
	if got := assignments(); got[node] != "active/true" || got[backupNode] != "active/false" {
		t.Fatalf("assignments after the edge observed the route = %v", got)
	}
	// Served only where observed: DNS and browser access can never name the unobserved edge.
	if got := served(); len(got) != 1 || got[0] != node+"/edge_ready" {
		t.Fatalf("preview_routes = %v, want only %s/edge_ready", got, node)
	}
	if err := gate(); err != nil {
		t.Fatalf("readiness with one serving edge = %v", err)
	}
	exec(`UPDATE preview_leases SET origin_state='ready' WHERE id=$1`, lease)
	if got := served(); len(got) != 1 || got[0] != node+"/ready" {
		t.Fatalf("preview_routes after origin ready = %v", got)
	}
	exec(`UPDATE endpoint_edge_assignments SET observed_at=now()-interval '31 seconds' WHERE endpoint_id=$1`, lease)
	if got := served(); len(got) != 0 {
		t.Fatalf("a route the edge stopped observing is still served: %v", got)
	}
	if err := service.Observe(ctx, edge, []runtimewire.Observation{runtimeObservation, observation}); err != nil {
		t.Fatal(err)
	}
	// A stale endpoint observation is dropped without failing the runtime observation beside it.
	stale := observation
	stale.AttachmentGeneration++
	if err := service.Observe(ctx, edge, []runtimewire.Observation{runtimeObservation, stale}); err != nil {
		t.Fatalf("stale endpoint observation failed the report: %v", err)
	}
	// Renewal by the same worker keeps the assignment and its observation.
	if _, err := service.Attach(ctx, proof, request); err != nil {
		t.Fatal(err)
	}
	if got := assignments(); got[node] != "active/true" {
		t.Fatalf("renewal reset a ready assignment: %v", got)
	}
	// The lease ends: the route leaves the edge snapshot at once and the next reconcile removes it.
	exec(`UPDATE preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, lease)
	if values, err := service.Pull(ctx, edge, capable); err != nil || len(values) != 1 {
		t.Fatalf("stopped preview still pulled: %d, %v", len(values), err)
	}
	if _, err := service.Attach(ctx, proof, request); err != nil {
		t.Fatal(err)
	}
	if got := assignments(); got[node] != "removed/false" || got[backupNode] != "removed/false" {
		t.Fatalf("assignments after the lease ended = %v", got)
	}
}

type attachmentReadinessFunc func() error

func (f attachmentReadinessFunc) RequirePreviewAttachmentReady(context.Context, string, string, string, string, string) error {
	return f()
}
