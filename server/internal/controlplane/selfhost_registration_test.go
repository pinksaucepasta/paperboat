package controlplane_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
)

func TestSelfhostControlNodeRegistrationFenceOnPostgres(t *testing.T) {
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
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	installationPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	installed := selfhost.Registered{InstallationID: "fence_install_" + time.Now().Format("150405.000000000"), NodeID: "fence_node_" + time.Now().Format("150405.000000000")}
	installed.EdgePool = installed.InstallationID
	registration := struct {
		EndpointHost      string
		TCPPort, QUICPort uint16
	}{"edge-fence.example.test", 27443, 27444}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,region,failure_domain,roles,transports,capacity_limit,capacity_observed_at,registry_expires_at,tls_spki_sha256) VALUES($1,$2,'1.0','initial_fixture',$3,$4,$5,'registered','test','test-a',ARRAY['edge'],ARRAY['http2','http3'],8,now(),now()+interval '1 hour',$6)`, installed.NodeID, installed.InstallationID, registration.EndpointHost, registration.TCPPort, registration.QUICPort, bytes.Repeat([]byte{0xbb}, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'ciphertext','registration fence','tunnel','account','fixture_account',$1,true)`, installed.InstallationID, installed.NodeID, []byte(installationPublic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, statement := range []struct {
			query string
			id    string
		}{
			{`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, installed.InstallationID},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, installed.NodeID},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, "managed-registration-fence"},
		} {
			if _, cleanupErr := database.SQL().ExecContext(context.Background(), statement.query, statement.id); cleanupErr != nil {
				t.Errorf("cleanup failed: %v", cleanupErr)
			}
		}
	})
	now := time.Now().UTC()
	base := dbsqlc.RegisterControlTunnelNodeParams{ID: installed.NodeID, EdgePool: installed.EdgePool, ProtocolVersion: "1.0", ProcessEpoch: "epoch-restart", EndpointHost: sql.NullString{String: registration.EndpointHost, Valid: true}, EndpointTcpPort: sql.NullInt32{Int32: int32(registration.TCPPort), Valid: true}, EndpointQuicPort: sql.NullInt32{Int32: int32(registration.QUICPort), Valid: true}, CarrierEndpointHost: sql.NullString{String: registration.EndpointHost, Valid: true}, CarrierEndpointTcpPort: sql.NullInt32{Int32: int32(registration.TCPPort), Valid: true}, CarrierEndpointQuicPort: sql.NullInt32{Int32: int32(registration.QUICPort), Valid: true}, CarrierServerSpkiSha256: sql.NullString{String: "sha256:" + strings.Repeat("b", 64), Valid: true}, CarrierServerCertificateChainPem: sql.NullString{String: "certificate", Valid: true}, Capacity: []byte(`{"connections":8}`), Now: sql.NullTime{Time: now, Valid: true}}
	if row, err := database.Queries().RegisterControlTunnelNode(ctx, base); err != nil || row.ID != installed.NodeID || row.EdgePool != installed.EdgePool {
		t.Fatalf("valid restart: row=%+v err=%v", row, err)
	}
	rejected := func(name string, mutate func(*dbsqlc.RegisterControlTunnelNodeParams)) {
		t.Helper()
		candidate := base
		candidate.ProcessEpoch = "rejected-" + name
		mutate(&candidate)
		if _, err := database.Queries().RegisterControlTunnelNode(ctx, candidate); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	rejected("host", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.EndpointHost.String = "other.example.test" })
	rejected("tcp", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.EndpointTcpPort.Int32++ })
	rejected("quic", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.EndpointQuicPort.Int32++ })
	rejected("pool", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.EdgePool = "other-pool" })
	rejected("carrier-host", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.CarrierEndpointHost.String = "other.example.test" })
	rejected("carrier-tcp", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.CarrierEndpointTcpPort.Int32 += 10 })
	rejected("carrier-quic", func(p *dbsqlc.RegisterControlTunnelNodeParams) { p.CarrierEndpointQuicPort.Int32 += 10 })
	rejected("carrier-pin", func(p *dbsqlc.RegisterControlTunnelNodeParams) {
		p.CarrierServerSpkiSha256.String = "sha256:" + strings.Repeat("a", 64)
	})
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET claim_complete=false WHERE id=$1`, installed.InstallationID); err != nil {
		t.Fatal(err)
	}
	rejected("unclaimed", func(*dbsqlc.RegisterControlTunnelNodeParams) {})
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET claim_complete=true WHERE id=$1`, installed.InstallationID); err != nil {
		t.Fatal(err)
	}

	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET state='removed' WHERE id=$1`, installed.InstallationID); err != nil {
		t.Fatal(err)
	}
	rejected("removed", func(*dbsqlc.RegisterControlTunnelNodeParams) {})
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET state='active',capability='relay' WHERE id=$1`, installed.InstallationID); err != nil {
		t.Fatal(err)
	}
	rejected("relay", func(*dbsqlc.RegisterControlTunnelNodeParams) {})
	managed := base
	managed.ID = "managed-registration-fence"
	managed.EdgePool = "managed"
	managed.EndpointHost.String = "managed.example.test"
	managed.CarrierEndpointHost.String = "managed.example.test"
	if _, err = database.Queries().RegisterControlTunnelNode(ctx, managed); err != nil {
		t.Fatalf("managed create: %v", err)
	}
	managed.EndpointHost.String = "managed-reconfigured.example.test"
	managed.CarrierEndpointHost.String = managed.EndpointHost.String
	managed.EdgePool = "managed-next"
	if row, e := database.Queries().RegisterControlTunnelNode(ctx, managed); e != nil || row.EdgePool != "managed-next" || row.EndpointHost.String != managed.EndpointHost.String {
		t.Fatalf("managed update: row=%+v err=%v", row, e)
	}
}
