package operatorinfra

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestManagedNodeRestartAfterGracefulDrain(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	region, node := "restart_region_"+suffix, "restart_node_"+suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.managed_node_authority WHERE node_id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_regions WHERE id=$1`, region)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_regions(id,display_name) VALUES($1,'Restart test')`, region); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,state,ready) VALUES($1,'managed:test','1.0','old_epoch','edge.example.test',27443,27444,'edge.example.test',27443,27444,'draining',false)`, node); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(node))
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,$3,$4,'{}')`, node, region, hash[:], []byte("test")); err != nil {
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
