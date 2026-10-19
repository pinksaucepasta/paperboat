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
	exec(`UPDATE paperboat.runtime_carrier_attachments SET active=false WHERE route_id=$1 AND active`, route)
	exec(`UPDATE paperboat.runtime_carrier_attachments SET active=true WHERE route_id=$1 AND edge_node_id=$2`, route, first)
	chosen, err = selector.SelectPreviewEdgeNode(context.Background(), PreviewEdgeNodeSelectionRequest{AccountID: account, PreviewID: "preview_" + stamp, OperationID: "operation_" + stamp, OwnerMachineID: machine, InstallationGeneration: 1})
	if err != nil || chosen != first {
		t.Fatalf("promoted preview selected %q, want %q: %v", chosen, first, err)
	}
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
