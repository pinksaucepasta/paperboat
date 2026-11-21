package operatorinfra

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

func TestManagedNodeRestartAfterGracefulDrain(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	region, node := "restart_region_"+suffix, "restart_node_"+suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_installations WHERE node_id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_regions WHERE id=$1`, region)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_regions(id,display_name) VALUES($1,'Restart test')`, region); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,state,ready) VALUES($1,'managed:test','1.0','old_epoch','edge.example.test',27443,27444,'edge.example.test',27443,27444,'draining',false)`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET region=$2 WHERE id=$1`, node, region); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(node))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,decode(md5($1)||md5($1),'hex'),$2,$3,'managed fixture','tunnel','global','global',$1,true)`, node, hash[:], []byte("test")); err != nil {
		t.Fatal(err)
	}
	registration := dbsqlc.RegisterControlTunnelNodeParams{ID: node, EdgePool: "managed:test", ProtocolVersion: "1.0", ProcessEpoch: "new_epoch", EndpointHost: sql.NullString{String: "edge.example.test", Valid: true}, EndpointTcpPort: sql.NullInt32{Int32: 27443, Valid: true}, EndpointQuicPort: sql.NullInt32{Int32: 27444, Valid: true}, CarrierEndpointHost: sql.NullString{String: "edge.example.test", Valid: true}, CarrierEndpointTcpPort: sql.NullInt32{Int32: 27443, Valid: true}, CarrierEndpointQuicPort: sql.NullInt32{Int32: 27444, Valid: true}, CarrierServerSpkiSha256: sql.NullString{String: "sha256:" + fmt.Sprintf("%064x", 1), Valid: true}, CarrierServerCertificateChainPem: sql.NullString{String: "certificate", Valid: true}, Capacity: []byte(`{"connectors":8}`), Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}}
	if _, err := store.Queries().RegisterControlTunnelNode(ctx, registration); err != nil {
		t.Fatalf("graceful restart failed: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='draining',drain_deadline=now()+interval '30 minutes' WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
	registration.ProcessEpoch = "operator_drain_rejected"
	if _, err := store.Queries().RegisterControlTunnelNode(ctx, registration); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("operator drain accepted: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='retired',drain_deadline=NULL WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Queries().RegisterControlTunnelNode(ctx, registration); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("retired node accepted: %v", err)
	}
}

// addManagedNode makes a managed node the way production does: a registered node row in region plus a global pbh
// installation claimed for it. It returns the installation's runtime credential; key is the Service's encryption key.
func addManagedNode(t *testing.T, store *db.DB, key, id, region, pool, domain, host string, roles, transports []string, capacity int64) string {
	t.Helper()
	ctx := context.Background()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	credential := hex.EncodeToString(raw)
	hash := sha256.Sum256([]byte(credential))
	cipher, err := secrets.Encrypt(key, credential)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE node_id=$1`, id)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, id)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
	})
	now := time.Now().UTC()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,$2,'1.0','',$3,24443,24444,$3,24443,24444,'registered',false,NULL,1,$4,$5,$6,$7,$8,0,$9,$9)`, id, pool, host, region, domain, roles, transports, capacity, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,decode(md5($1)||md5($1),'hex'),$2,$3,'managed fixture',$4,'global','global',$1,true)`, id, hash[:], cipher, map[bool]string{true: "relay", false: "tunnel"}[len(roles) == 1 && roles[0] == "relay"]); err != nil {
		t.Fatal(err)
	}
	if len(roles) == 1 && roles[0] == "edge" {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES($1,$2,$3,$4,$5)`, "managed:"+id, id, make([]byte, 32), now, now.AddDate(10, 0, 0)); err != nil {
			t.Fatal(err)
		}
	}
	return credential
}
