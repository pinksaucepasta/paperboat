package previewattachment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/edgeplacement"
)

func TestPreviewEdgePlacementQueryOnPostgres(t *testing.T) {
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
	if err = db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	selector, err := NewDBPreviewEdgeNodeSelector(database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: "missing_account", PreviewID: "missing_preview", OperationID: "missing_operation", OwnerMachineID: "missing_machine", InstallationGeneration: 1})
	if !errors.Is(err, ErrAdmissionUnavailable) {
		t.Fatalf("preview placement query: %v", err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	account, machine := "preview_place_account_"+stamp, "preview_place_machine_"+stamp
	environment, route := "preview_place_env_"+stamp, "preview_place_route_"+stamp
	first, preferred := "preview_place_a_"+stamp, "preview_place_z_"+stamp
	defer func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.preview_leases WHERE account_id=$1`, account)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.runtime_carrier_attachments WHERE route_id=$1`, route)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_routes WHERE id=$1`, route)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_environments WHERE id=$1`, environment)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
		for _, id := range []string{first, preferred} {
			_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		}
	}()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, environment, account)
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := base64.RawURLEncoding.EncodeToString(publicKey)
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,installation_generation,worker_generation,public_identity_key) VALUES ($1,$2,$3,'preview-place','linux','amd64','/workspace','online','occupied',1,1,$4)`, machine, account, environment, identity)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,desired_state) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080,1,'attached')`, route, environment, machine+".runtime.example.test")
	for _, node := range []struct{ id, epoch, domain string }{{first, "epoch-aa", "domain-a"}, {preferred, "epoch-zz", "domain-z"}} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem) VALUES($1,'default','1.0',$2,'ready',true,now(),'region',$3,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,'sha256:'||repeat('a',64),'pem')`, node.id, node.epoch, node.domain)
	}
	exec(`INSERT INTO paperboat.machine_edge_placements(machine_id,account_id,installation_generation,worker_generation,primary_node_id,primary_process_epoch,secondary_node_id,secondary_process_epoch,observed_at) VALUES($1,$2,1,1,$3,'epoch-zz',$4,'epoch-aa',now())`, machine, account, preferred, first)
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,'process',1,$4,'epoch-zz',$5,1,'session_01234567-89ab-4cde-8fab-0123456789ab','session_11234567-89ab-4cde-8fab-0123456789ab','{}',now()+interval '1 hour',true,now(),true),($1,$2,$3,1,1,'process',1,$6,'epoch-aa',$5,1,'session_21234567-89ab-4cde-8fab-0123456789ab','session_31234567-89ab-4cde-8fab-0123456789ab','{}',now()+interval '1 hour',true,now(),false)`, route, machine, account, preferred, identity, first)
	exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline) VALUES($1,$2,$3,$3,$4,'session','foreground','http','127.0.0.1:3000',now()+interval '1 hour')`, "preview_"+stamp, "https://fixture.preview.example.test", account, machine)
	chosen, err := selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	if err != nil || chosen != preferred {
		t.Fatalf("preview selected %q, want preferred %q: %v", chosen, preferred, err)
	}
	// A heartbeat or observation that is updating the preferred edge's row must not push the placement
	// to the other edge: the published DNS record would then name an edge without the route.
	busy, err := database.SQL().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = busy.ExecContext(context.Background(), `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now() WHERE id=$1`, preferred); err != nil {
		t.Fatal(err)
	}
	chosen, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	_ = busy.Rollback()
	if err != nil || chosen != preferred {
		t.Fatalf("placement skipped the edge whose row was being updated: selected %q, want %q: %v", chosen, preferred, err)
	}
	exec(`UPDATE paperboat.runtime_carrier_attachments SET active=false WHERE route_id=$1 AND active`, route)
	exec(`UPDATE paperboat.runtime_carrier_attachments SET active=true WHERE route_id=$1 AND edge_node_id=$2`, route, first)
	chosen, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	if err != nil || chosen != first {
		t.Fatalf("promoted preview selected %q, want %q: %v", chosen, first, err)
	}
	// The second attachment of a preview lives on the other edge: it needs a live first attachment, takes
	// an edge in another failure domain, and neither slot takes the edge the other holds.
	second := PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp + SecondaryOperationSuffix, OwnerMachineID: machine, InstallationGeneration: 1}
	if _, err = selector.SelectPreviewEdgeNode(context.Background(), second); !errors.Is(err, ErrAdmissionUnavailable) {
		t.Fatalf("a second attachment without a first was placed: %v", err)
	}
	attach := func(operation, edge string, slot int) {
		t.Helper()
		exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id,completed_at) VALUES($1,$2,$1,decode(repeat('ab',32),'hex'),'preview.attachment_secondary','preview_lease',$3,'ready','succeeded',100,'unchanged',$1,now()) ON CONFLICT DO NOTHING`, operation, account, "preview_"+stamp)
		exec(`INSERT INTO paperboat.preview_lease_create_operations(account_id,preview_id,operation_id,slot) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, account, "preview_"+stamp, operation, slot)
		exec(`INSERT INTO paperboat.preview_lease_carrier_attachments(account_id,preview_id,operation_id,idempotency_key,request_id,correlation_id,request_hash,owner_machine_id,owner_session_id,host_id,edge_node_id,machine_identity_public_key,machine_identity_thumbprint,lease_generation,tunnel_id,connector_id,connector_session_id,process_generation,config_generation,config_content_hash,route_id,route_generation,edge_endpoints,edge_process_epoch,edge_carrier_server_spki_sha256,edge_carrier_server_certificate_chain_pem,state,issued_at,expires_at,edge_slot)
 VALUES($1,$2,$3,$3,$3,$3,decode(repeat('ab',32),'hex'),$4,'session',$4,$5,$6,$7,1,'tunnel_x','connector_x','session_x',1,1,'sha256:'||repeat('a',64),$8,1,ARRAY['tcp://edge.example.test:24001','quic://edge.example.test:24002'],$9,'sha256:'||repeat('b',64),'test-certificate','pending',now(),now()+interval '1 hour',$10)`,
			account, "preview_"+stamp, operation, machine, edge, identity, "sha256:"+base64.RawURLEncoding.EncodeToString(make([]byte, 32)), route, map[string]string{first: "epoch-aa", preferred: "epoch-zz"}[edge], slot)
	}
	attach("operation_"+stamp, first, 0)
	other, err := selector.SelectPreviewEdgeNode(context.Background(), second)
	if err != nil || other != preferred {
		t.Fatalf("second attachment placed on %q (%v), want the other edge %q", other, err, preferred)
	}
	attach("operation_"+stamp+SecondaryOperationSuffix, preferred, 1)
	chosen, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	if err != nil || chosen != first {
		t.Fatalf("first attachment moved to %q (%v) while the second holds the other edge", chosen, err)
	}
	exec(`UPDATE paperboat.preview_lease_carrier_attachments SET state='released',released_at=now() WHERE account_id=$1 AND preview_id=$2`, account, "preview_"+stamp)
	// A published URL stays on the edge its DNS record names even when the other edge's carrier is the
	// active one: a resolver that cached the record would otherwise reach an edge without the route.
	exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_ipv4=$2 WHERE id=$1`, preferred, "192.0.2.77")
	exec(`INSERT INTO paperboat.edge_dns_publications(hostname,owner_id,resource_generation,readiness_version,observed_at,publication_generation,desired_addresses,provider_records,state,created_at,updated_at) VALUES('fixture.preview.example.test',$1,1,'v',now(),1,'["192.0.2.77"]','[]','submitted',now(),now())`, "preview_"+stamp)
	defer func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.edge_dns_publications WHERE hostname='fixture.preview.example.test'`)
	}()
	chosen, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	if err != nil || chosen != preferred {
		t.Fatalf("placement left the published edge: selected %q, want %q: %v", chosen, preferred, err)
	}
	exec(`DELETE FROM paperboat.edge_dns_publications WHERE hostname='fixture.preview.example.test'`)
	// An edge outside the workspace's edge set takes no NEW placement, but a machine
	// whose carrier is already active there keeps working until it reattaches: invalidating the
	// attachment at once stranded the machine (preview create failed with edge_admission_unavailable
	// and dispatch got EOF) for as long as its next reattach took.
	exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,node_ids) VALUES('account',$1,'managed-only',ARRAY[$2]::text[])`, account, preferred)
	defer func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, account)
	}()
	chosen, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	if err != nil || chosen != first {
		t.Fatalf("machine carrier on an edge outside the workspace edge set was stranded: selected %q err=%v", chosen, err)
	}
	var valid bool
	if err = database.SQL().QueryRowContext(context.Background(), `SELECT paperboat.runtime_carrier_valid($1,$2,'epoch-aa',true)`, route, first).Scan(&valid); err != nil || !valid {
		t.Fatalf("existing runtime attachment on an edge outside the workspace edge set valid=%v err=%v", valid, err)
	}
	var offered []string
	err = database.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
		candidates, candidateErr := edgeplacement.Candidates(ctx, tx, account, sql.NullString{}, time.Now().UTC())
		for _, candidate := range candidates {
			offered = append(offered, candidate.NodeID)
		}
		return candidateErr
	})
	if err != nil || len(offered) != 1 || offered[0] != preferred {
		t.Fatalf("new managed placement offered %v err=%v, want only %q (the workspace edge set)", offered, err, preferred)
	}
}
