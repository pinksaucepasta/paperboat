package operatorinfra

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
)

func TestOperatorInfrastructureEnrollmentReplayAndRegionFence(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "infra_admin_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, admin); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, "operator-infrastructure-test-key-32-bytes")
	region, err := s.MutateRegion(ctx, admin, "region-create-"+x, RegionMutation{Action: "create", ID: "region_" + x, DisplayName: "Test region", Reason: "case:test"})
	if err != nil || !region.Enabled || region.Revision != 1 {
		t.Fatalf("region=%#v err=%v", region, err)
	}
	nodeInput := NodeMutation{Action: "enroll", NodeID: "managed_" + x, RegionID: region.ID, EdgePool: "managed:" + region.ID, FailureDomain: "zone-a", EndpointHost: "edge.example.test", EndpointTCPPort: 24443, EndpointQUICPort: 24444, Reason: "case:test", Roles: []string{"edge"}, Transports: []string{"http3"}, CapacityLimit: 8, UsagePublicKey: make([]byte, 32)}
	enrolled, err := s.MutateNode(ctx, admin, "node-enroll-"+x, nodeInput)
	if err != nil || enrolled.RuntimeCredential == "" || enrolled.Node.Ready || enrolled.Node.Revision != 1 {
		t.Fatalf("enroll=%#v err=%v", enrolled, err)
	}
	replay, err := s.MutateNode(ctx, admin, "node-enroll-"+x, nodeInput)
	if err != nil || replay.RuntimeCredential != enrolled.RuntimeCredential || replay.Node.ID != enrolled.Node.ID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if node, err := s.RuntimeNode(ctx, enrolled.RuntimeCredential); err != nil || node != nodeInput.NodeID {
		t.Fatalf("runtime node=%q err=%v", node, err)
	}
	now := time.Now().UTC()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{nodeInput.EndpointHost}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(spki)
	pin := "sha256:" + hex.EncodeToString(digest[:])
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}))
	registration := dbsqlc.RegisterControlTunnelNodeParams{ID: nodeInput.NodeID, EdgePool: nodeInput.EdgePool, ProtocolVersion: "1.0", ProcessEpoch: "managed-runtime", EndpointHost: sql.NullString{String: nodeInput.EndpointHost, Valid: true}, EndpointTcpPort: sql.NullInt32{Int32: nodeInput.EndpointTCPPort, Valid: true}, EndpointQuicPort: sql.NullInt32{Int32: nodeInput.EndpointQUICPort, Valid: true}, CarrierEndpointHost: sql.NullString{String: nodeInput.EndpointHost, Valid: true}, CarrierEndpointTcpPort: sql.NullInt32{Int32: nodeInput.EndpointTCPPort, Valid: true}, CarrierEndpointQuicPort: sql.NullInt32{Int32: nodeInput.EndpointQUICPort, Valid: true}, CarrierServerSpkiSha256: sql.NullString{String: pin, Valid: true}, CarrierServerCertificateChainPem: sql.NullString{String: chain, Valid: true}, Capacity: []byte(`{"connections":8}`), Now: sql.NullTime{Time: now, Valid: true}}
	edge := controlplane.NewEdgeService(store, strings.Repeat("fleet-test", 4))
	edge.SetNodeCredentials(nodeauth.Composite{s})
	runtimeHandler := edge.Handler()
	endpoint := map[string]any{"host": nodeInput.EndpointHost, "tcp_port": nodeInput.EndpointTCPPort, "quic_port": nodeInput.EndpointQUICPort}
	body, _ := json.Marshal(map[string]any{"edge_node_id": nodeInput.NodeID, "edge_pool": nodeInput.EdgePool, "protocol": "1.0", "process_epoch": registration.ProcessEpoch, "capacity": 8, "connector_endpoint": endpoint, "carrier_endpoint": endpoint, "carrier_server_spki_sha256": pin, "carrier_server_certificate_chain_pem": chain})
	runtimeRequest := httptest.NewRequest("POST", "/v1/nodes/register", strings.NewReader(string(body)))
	runtimeRequest.Header.Set("Authorization", "Bearer "+enrolled.RuntimeCredential)
	runtimeRequest.Header.Set("Content-Type", "application/json")
	runtimeResponse := httptest.NewRecorder()
	runtimeHandler.ServeHTTP(runtimeResponse, runtimeRequest)
	if runtimeResponse.Code != 204 && runtimeResponse.Code != 200 {
		t.Fatalf("scoped register: %d %s", runtimeResponse.Code, runtimeResponse.Body.String())
	}
	changed := registration
	changed.ProcessEpoch = "malicious"
	changed.EdgePool = "other"
	if _, err = store.Queries().RegisterControlTunnelNode(ctx, changed); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("managed pool mutation accepted: %v", err)
	}
	if _, err = store.Queries().HeartbeatControlTunnelNode(ctx, dbsqlc.HeartbeatControlTunnelNodeParams{ID: nodeInput.NodeID, ProcessEpoch: registration.ProcessEpoch, Ready: true, Observation: []byte(`{}`), Now: sql.NullTime{Time: now, Valid: true}}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	rotate := NodeMutation{Action: "rotate", NodeID: nodeInput.NodeID, Reason: "case:test", Revision: enrolled.Node.Revision}
	rotated, err := s.MutateNode(ctx, admin, "node-rotate-"+x, rotate)
	if err != nil || rotated.RuntimeCredential == "" || rotated.RuntimeCredential == enrolled.RuntimeCredential {
		t.Fatalf("rotate=%#v err=%v", rotated, err)
	}
	if _, err = s.RuntimeNode(ctx, enrolled.RuntimeCredential); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatalf("old credential=%v", err)
	}
	rotateReplay, err := s.MutateNode(ctx, admin, "node-rotate-"+x, rotate)
	if err != nil || rotateReplay.RuntimeCredential != rotated.RuntimeCredential {
		t.Fatalf("rotate replay=%#v err=%v", rotateReplay, err)
	}
	enrollReplay, err := s.MutateNode(ctx, admin, "node-enroll-"+x, nodeInput)
	if err != nil || enrollReplay.RuntimeCredential != enrolled.RuntimeCredential {
		t.Fatalf("enroll replay changed=%#v err=%v", enrollReplay, err)
	}
	page, err := s.ListNodes(ctx, admin, region.ID, "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("nodes=%#v err=%v", page, err)
	}
	disabled, err := s.MutateRegion(ctx, admin, "region-disable-"+x, RegionMutation{Action: "disable", ID: region.ID, Reason: "case:test", Revision: region.Revision})
	if err != nil || disabled.Enabled {
		t.Fatalf("disable=%#v err=%v", disabled, err)
	}
	if _, err = s.RuntimeNode(ctx, rotated.RuntimeCredential); !errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatalf("disabled credential=%v", err)
	}
	if _, err = store.Queries().HeartbeatControlTunnelNode(ctx, dbsqlc.HeartbeatControlTunnelNodeParams{ID: nodeInput.NodeID, ProcessEpoch: registration.ProcessEpoch, Ready: true, Observation: []byte(`{}`), Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled heartbeat accepted: %v", err)
	}
	var state string
	var ready bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT state,ready FROM paperboat.control_tunnel_nodes WHERE id=$1`, nodeInput.NodeID).Scan(&state, &ready); err != nil || state != "draining" || ready {
		t.Fatalf("disabled node=%s/%t err=%v", state, ready, err)
	}
	if _, err = s.MutateNode(ctx, admin, "node-resume-"+x, NodeMutation{Action: "resume", NodeID: nodeInput.NodeID, Reason: "case:test", Revision: rotated.Node.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled resume=%v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='support' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MutateRegion(ctx, admin, "region-disable-"+x, RegionMutation{Action: "disable", ID: region.ID, Reason: "case:test", Revision: region.Revision}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("replay after role revocation=%v", err)
	}
}

func infraTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}
