package connectorprotocol

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func TestPublicPreviewIngressPostgres(t *testing.T) {
	f := newTRK08PostgresFixture(t)
	ctx := context.Background()
	c := f.connectors[0]
	now := f.clock.now
	node := "edge_preview_" + f.suffix
	epoch := "epoch_preview_" + f.suffix
	preview := "preview_usage_" + f.suffix
	operation := "operation_preview_" + f.suffix
	route := "route_preview_" + f.suffix
	env := "env_" + c.hostID
	hostname := "usage-" + strings.ReplaceAll(f.suffix, "_", "-") + ".preview.example.test"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM control_usage_receipts WHERE operation_id=$1`, `DELETE FROM resource_bandwidth_events WHERE operation_id='edge:'||$1`} {
			_, _ = f.database.SQL().ExecContext(ctx, q, operation)
		}
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM preview_leases WHERE id=$1`, preview)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, node)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, f.accountID)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM billing_workspaces WHERE user_id=$1`, f.accountID)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM machine_control_sessions WHERE machine_id=$1`, c.hostID)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM control_environments WHERE id=$1`, env)
	})
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, env, c.hostID, f.accountID)
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,registry_expires_at,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at) VALUES($1,'test','1.0',$2,'ready',true,$3,ARRAY['edge'],$3::timestamptz+interval '1 hour','test','test',ARRAY['http3','http2'],100,0,$3)`, node, epoch, now)
	exec(`INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) SELECT $1,id,installation_generation,$1,$3,$4,1 FROM user_machines WHERE id=$2`, operation, c.hostID, now.Add(-time.Minute), now.Add(time.Hour))
	exec(`INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, operation)
	exec(`INSERT INTO preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,created_at,last_renewed_at) VALUES('foreground',$1,$2,$3,$3,$4,$5,'http','127.0.0.1:3000','public',$6,$7,$7)`, preview, "https://"+hostname, f.accountID, c.hostID, c.oldSessionID, now.Add(time.Hour), now)
	exec(`INSERT INTO operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,retrying,outcome,correlation_id,created_at,updated_at) VALUES($1,$2,$1,decode(repeat('ab',32),'hex'),'preview.create','preview_lease',$3,'connecting','running',60,false,'changed',$1,$4,$4)`, operation, f.accountID, preview, now)
	exec(`INSERT INTO preview_lease_carrier_attachments(account_id,preview_id,operation_id,idempotency_key,request_id,correlation_id,request_hash,owner_machine_id,owner_session_id,host_id,edge_node_id,machine_identity_public_key,machine_identity_thumbprint,lease_generation,tunnel_id,connector_id,connector_session_id,process_generation,config_generation,config_content_hash,route_id,route_generation,edge_endpoints,edge_process_epoch,edge_carrier_server_spki_sha256,edge_carrier_server_certificate_chain_pem,state,edge_ready,origin_ready,ready_at,issued_at,expires_at)
 SELECT $1,$2,$3,$3,$3,$3,decode(repeat('ab',32),'hex'),m.id,$4,m.id,$5,m.public_identity_key,$6,1,$7,$8,$4,1,1,'sha256:'||repeat('a',64),$9,1,ARRAY['tcp://edge.example.test:24001','quic://edge.example.test:24002'],$10,'sha256:'||repeat('b',64),'test-certificate','ready',true,true,$11,$11,$12 FROM user_machines m WHERE m.id=$13`, f.accountID, preview, operation, c.oldSessionID, node, "sha256:"+c.oldThumbprint, f.tunnelID, c.id, route, epoch, now, now.Add(time.Hour), c.hostID)
	source := SQLIngressSource{DB: f.database, Clock: f.clock}
	check := func(want int) {
		t.Helper()
		got, err := source.Edge(ctx, node, epoch)
		if err != nil || len(got) != want {
			t.Fatalf("public preview decisions=%d want=%d: %v", len(got), want, err)
		}
		if want == 1 {
			d := got[0]
			if d.Binding.PublicationID != preview || d.Binding.RouteID != route || d.Binding.Hostname != hostname || d.Binding.Lifecycle != wire.TunnelEphemeral || d.Binding.EnvironmentID != env || d.Binding.HostID != c.hostID || d.Validate(now) != nil {
				t.Fatal("incorrect preview authority")
			}
		}
	}
	check(1)
	for _, test := range []struct{ deny, restore string }{
		{`UPDATE control_tunnel_nodes SET process_epoch='epoch_other' WHERE id=$1`, `UPDATE control_tunnel_nodes SET process_epoch=$2,last_heartbeat_at=$3 WHERE id=$1`},
		{`UPDATE control_tunnel_nodes SET last_heartbeat_at=now()-interval '1 minute' WHERE id=$1`, `UPDATE control_tunnel_nodes SET process_epoch=$2,last_heartbeat_at=$3 WHERE id=$1`},
	} {
		exec(test.deny, node)
		check(0)
		exec(test.restore, node, epoch, now)
		check(1)
	}
	exec(`UPDATE preview_leases SET access_mode='private' WHERE id=$1`, preview)
	check(0)
	exec(`UPDATE preview_leases SET access_mode='public' WHERE id=$1`, preview)
	check(1)
	exec(`UPDATE user_machines SET revoked_at=$2 WHERE id=$1`, c.hostID, now)
	check(0)
	exec(`UPDATE user_machines SET revoked_at=NULL WHERE id=$1`, c.hostID)
	check(1)
	exec(`UPDATE preview_leases SET access_mode='private' WHERE id=$1`, preview)
	q := wire.PrivateAccessRequest{AccountID: f.accountID, ResourceKind: "preview", ResourceID: preview, RouteID: route, Audience: "paperboat-preview-http", MachineID: c.hostID, SessionID: "installation_" + f.suffix, InstallationGeneration: 1, ExpiresAt: now.Add(time.Minute), Nonce: "nonce_" + f.suffix, OperationID: operation, CarrierSessionID: c.oldSessionID, RouteGeneration: 1, ProcessGeneration: 1, ConfigGeneration: 1, SessionGeneration: 1, AssignmentGeneration: 1, EdgeNodeID: node, EdgeProcessEpoch: epoch, Protocol: "http", Method: "CONNECT", Host: hostname, Path: "/", IdempotencyKey: "access_" + f.suffix, RequestID: "request_" + f.suffix, CorrelationID: "correlation_" + f.suffix}
	evidence := wire.PrivateAccessOpen{Schema: wire.PrivateAccessSchema, Kind: wire.PrivateAccessKind, Grant: "signed-grant", Request: q}
	native, err := source.RestrictedPreview(ctx, node, epoch, evidence)
	if err != nil || native.Validate(now) != nil || native.NativeAuthorization == nil || native.NativeAuthorization.Request != q {
		t.Fatalf("native preview authority: %v", err)
	}
	changed := evidence
	changed.Request.OperationID = "operation_wrong"
	if _, err := source.RestrictedPreview(ctx, node, epoch, changed); err == nil {
		t.Fatal("wrong preview operation admitted")
	}
	var nativeWorkspace string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT workspace_id FROM usage_accounting_authorities WHERE authority_id=$1 AND edge_node_id=$2 AND route_id=$3`, "grant:"+q.Nonce, node, route).Scan(&nativeWorkspace); err != nil || nativeWorkspace != "personal:"+f.accountID {
		t.Fatalf("native workspace %q err=%v", nativeWorkspace, err)
	}

	var workspace string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT workspace_id FROM usage_accounting_authorities WHERE authority_id=$1 AND edge_node_id=$2 AND route_id=$3`, "public:"+preview+":1", node, route).Scan(&workspace); err != nil || workspace != "personal:"+f.accountID {
		t.Fatalf("accounting workspace=%q err=%v", workspace, err)
	}
	exec(`INSERT INTO control_usage_receipts(operation_id,edge_node_id,counter_epoch,environment_id,route_id,route_revision,direction,observed_bytes,delta_bytes,interval_start,interval_end,workspace_id) VALUES($1,$2,$3,$4,$5,1,'egress',32768,32768,$6,$7,$8)`, operation, node, epoch, env, route, now, now.Add(time.Second), workspace)
	exec(`SELECT paperboat.project_edge_bandwidth($1)`, operation)
	exec(`SELECT paperboat.project_edge_bandwidth($1)`, operation)
	var bytes int64
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT download_bytes FROM resource_bandwidth_events WHERE operation_id='edge:'||$1 AND workspace_id=$2 AND resource_kind='preview' AND resource_id=$3 AND mode='edge' AND hosting='managed' AND node_id=$4`, operation, workspace, preview, node).Scan(&bytes); err != nil || bytes != 32768 {
		t.Fatalf("preview projection bytes=%d err=%v", bytes, err)
	}
	// A verified customer alias authorizes that alias alone on a private edge.
	managedHostname := hostname
	hostname = "custom-" + strings.ReplaceAll(f.suffix, "_", "-") + ".example.test"
	installation := "installation_preview_" + f.suffix
	identity := sha256.Sum256([]byte(installation))
	t.Cleanup(func() {
		for _, query := range []string{`DELETE FROM selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, `DELETE FROM selfhost_installations WHERE scope_kind='account' AND scope_id=$1`} {
			if _, err := f.database.SQL().ExecContext(context.Background(), query, f.accountID); err != nil {
				t.Error(err)
			}
		}
	})
	exec(`UPDATE preview_leases SET access_mode='public' WHERE id=$1`, preview)
	exec(`UPDATE control_tunnel_nodes SET tls_spki_sha256=$2,public_ingress_ipv4='140.245.11.41',public_ingress_verified_at=now(),last_heartbeat_at=now(),endpoint_host='edge.example.test',endpoint_tcp_port=27443,endpoint_quic_port=27444 WHERE id=$1`, node, identity[:])
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,$3,'Ingress fixture','tunnel','account',$4,$1,true)`, installation, node, identity[:], f.accountID)
	exec(`INSERT INTO selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2])`, f.accountID, installation)
	store, err := previewtunnelstore.New(f.database)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.GetPreviewLeaseV1(ctx, f.accountID, preview)
	if err != nil {
		t.Fatal(err)
	}
	if lease.EdgeMode != previewtunnelstore.EdgeModeSelfhost || lease.PublicEndpoint == nil || *lease.PublicEndpoint != "" || lease.Endpoint != "https://"+managedHostname {
		t.Fatalf("unverified selfhost presentation/identity = %+v", lease)
	}
	exec(`INSERT INTO preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,expected_records,certificate_strategy,certificate_state,certificate_reference,caa_state,conflict_state) VALUES($1,$2,$3,1,$4,'exact','dns:ingress','verified','140.245.11.41',$5::jsonb,'managed','ready','certificate:fixture','ready','clear')`, "domain_preview_"+f.suffix, f.accountID, preview, hostname, `[{"type":"A","value":"140.245.11.41"}]`)
	lease, err = store.GetPreviewLeaseV1(ctx, f.accountID, preview)
	if err != nil {
		t.Fatal(err)
	}
	if lease.PublicEndpoint == nil || *lease.PublicEndpoint != "https://"+hostname || lease.Endpoint != "https://"+managedHostname {
		t.Fatalf("ready alias presentation/identity = %+v", lease)
	}
	check(1)
	exec(`UPDATE preview_leases SET access_mode='private' WHERE id=$1`, preview)
	evidence.Request.Host = managedHostname
	if _, err := source.RestrictedPreview(ctx, node, epoch, evidence); err == nil {
		t.Fatal("selfhost private grant authorized Paperboat hostname")
	}
	evidence.Request.Host = hostname
	if _, err := source.RestrictedPreview(ctx, node, epoch, evidence); err != nil {
		t.Fatalf("selfhost private custom alias denied: %v", err)
	}

	exec(`UPDATE preview_domains SET preview_generation=2 WHERE id=$1`, "domain_preview_"+f.suffix)
	lease, err = store.GetPreviewLeaseV1(ctx, f.accountID, preview)
	if err != nil {
		t.Fatal(err)
	}
	if lease.PublicEndpoint == nil || *lease.PublicEndpoint != "" {
		t.Fatal("stale-generation alias exposed as preview endpoint")
	}

}
