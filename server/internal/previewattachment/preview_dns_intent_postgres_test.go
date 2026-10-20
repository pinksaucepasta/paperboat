package previewattachment

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

// A preview's URL is known to the client the moment the lease exists, seconds before the host attaches.
// A resolver that looks the name up in that gap caches the shared wildcard answer (which can be the other
// edge) for its TTL, so the record must already point at the machine's primary edge.
func TestPreviewDNSIsPublishableBeforeTheHostAttachesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	account, machine, environment, route := "intent_account_"+stamp, "intent_machine_"+stamp, "intent_env_"+stamp, "intent_route_"+stamp
	primary, standby := "intent_a_"+stamp, "intent_z_"+stamp
	preview, hostname := "intent_preview_"+stamp, "intent-"+stamp+".preview.example.test"
	certificate := "intent_cert_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.preview_leases WHERE account_id=$1`, account)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.runtime_carrier_attachments WHERE route_id=$1`, route)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_routes WHERE id=$1`, route)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnel_certificate_records WHERE id=$1`, certificate)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_environments WHERE id=$1`, environment)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
		for _, id := range []string{primary, standby} {
			_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		}
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, environment, account)
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := base64.RawURLEncoding.EncodeToString(publicKey)
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,installation_generation,worker_generation,public_identity_key) VALUES ($1,$2,$3,'intent','linux','amd64','/workspace','online','occupied',1,1,$4)`, machine, account, environment, identity)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,desired_state,managed_public_host) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080,1,'attached',$3)`, route, environment, machine+".runtime.example.test")
	for index, node := range []struct{ id, epoch, domain, address string }{{primary, "epoch-pp", "domain-a", "192.0.2.10"}, {standby, "epoch-ss", "domain-z", "192.0.2.11"}} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,public_ingress_ipv4,public_ingress_verified_at) VALUES($1,'default','1.0',$2,'ready',true,now(),'region',$3,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,$4::inet,now())`, node.id, node.epoch, node.domain, node.address)
		exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,'process',1,$4,$5,$6,1,$7,$8,'{}',now()+interval '1 hour',true,now(),$9)`, route, machine, account, node.id, node.epoch, identity, fmt.Sprintf("session_%d1234567-89ab-4cde-8fab-0123456789ab", index), fmt.Sprintf("session_%d9234567-89ab-4cde-8fab-0123456789ab", index), index == 0)
	}
	exec(`INSERT INTO paperboat.tunnel_platform_certificate_targets(id,kind,hostname,account_id,challenge_reference) VALUES('platform_cert_public_v1','public_wildcard','*.preview.example.test','platform_system_v1','dns:fixture') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO paperboat.tunnel_certificate_records(id,domain_id,account_id,hostname,domain_generation,certificate_generation,strategy,state,certificate_reference,master_key_reference,certificate_ciphertext,private_key_ciphertext,fingerprint,issuer,not_before,expires_at,renewal_at,target_kind) VALUES($1,'platform_cert_public_v1','platform_system_v1','*.preview.example.test',1,1,'platform_dns01','active','fixture','fixture',decode(repeat('ab',29),'hex'),decode(repeat('ab',29),'hex'),decode(repeat('ab',32),'hex'),'fixture',now()-interval '1 hour',now()+interval '1 hour',now()-interval '1 hour','platform_wildcard')`, certificate)
	for _, node := range []struct{ id, epoch string }{{primary, "epoch-pp"}, {standby, "epoch-ss"}} {
		exec(`INSERT INTO paperboat.tunnel_certificate_edge_distributions(certificate_id,edge_node_id,edge_process_epoch,edge_assignment_generation,state,observed_certificate_generation) VALUES($1,$2,$3,1,'active',1)`, certificate, node.id, node.epoch)
	}
	exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline) VALUES($1,$2,$3,$3,$4,'session','foreground','http','127.0.0.1:3000',now()+interval '1 hour')`, preview, "https://"+hostname, account, machine)

	rows := func() []dbsqlc.ListReadyEdgeDNSAddressesV1Row {
		t.Helper()
		got, err := database.Queries().ListReadyEdgeDNSAddressesV1(ctx, dbsqlc.ListReadyEdgeDNSAddressesV1Params{TunnelID: preview, Hostname: hostname, Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := rows(); len(got) != 1 || got[0].ID != primary || got[0].Ipv4 != "192.0.2.10" || !strings.Contains(got[0].ReadinessVersion, "/intent/") {
		t.Fatalf("a new preview is advertised on %+v, want only the machine's primary edge", got)
	}
	exec(`UPDATE paperboat.preview_leases SET created_at=now()-interval '3 minutes',lease_deadline=now()+interval '1 hour' WHERE id=$1`, preview)
	if got := rows(); len(got) != 0 {
		t.Fatalf("a preview that never attached stayed advertised after two minutes: %+v", got)
	}
	exec(`UPDATE paperboat.preview_leases SET created_at=now() WHERE id=$1`, preview)
	exec(`UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, preview)
	if got := rows(); len(got) != 0 {
		t.Fatalf("a stopped preview was advertised: %+v", got)
	}
}
