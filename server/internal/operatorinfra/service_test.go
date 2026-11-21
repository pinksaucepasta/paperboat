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
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

func TestOperatorInfrastructureRegionFence(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "infra_admin_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, admin); err != nil {
		t.Fatal(err)
	}
	key := "operator-infrastructure-test-key-32-bytes"
	s := NewService(store, key)
	region, err := s.MutateRegion(ctx, admin, "region-create-"+x, RegionMutation{Action: "create", ID: "region_" + x, DisplayName: "Test region", Reason: "case:test"})
	if err != nil || !region.Enabled || region.Revision != 1 {
		t.Fatalf("region=%#v err=%v", region, err)
	}
	nodeInput := struct {
		NodeID, EdgePool, EndpointHost    string
		EndpointTCPPort, EndpointQUICPort int32
	}{"managed_" + x, "managed:" + region.ID, "edge.example.test", 24443, 24444}
	cred := addManagedNode(t, store, key, nodeInput.NodeID, region.ID, nodeInput.EdgePool, "zone-a", nodeInput.EndpointHost, []string{"edge"}, []string{"http3"}, 8)
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
	edge.SetNodeCredentials(nodeauth.Composite{selfhost.New(store, key)})
	runtimeHandler := edge.Handler()
	endpoint := map[string]any{"host": nodeInput.EndpointHost, "tcp_port": nodeInput.EndpointTCPPort, "quic_port": nodeInput.EndpointQUICPort}
	body, _ := json.Marshal(map[string]any{"edge_node_id": nodeInput.NodeID, "edge_pool": nodeInput.EdgePool, "protocol": "1.0", "process_epoch": registration.ProcessEpoch, "capacity": 8, "connector_endpoint": endpoint, "carrier_endpoint": endpoint, "carrier_server_spki_sha256": pin, "carrier_server_certificate_chain_pem": chain})
	runtimeRequest := httptest.NewRequest("POST", "/v1/nodes/register", strings.NewReader(string(body)))
	runtimeRequest.Header.Set("Authorization", "Bearer "+cred)
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
	page, err := s.ListNodes(ctx, admin, region.ID, "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("nodes=%#v err=%v", page, err)
	}
	disabled, err := s.MutateRegion(ctx, admin, "region-disable-"+x, RegionMutation{Action: "disable", ID: region.ID, Reason: "case:test", Revision: region.Revision})
	if err != nil || disabled.Enabled {
		t.Fatalf("disable=%#v err=%v", disabled, err)
	}
	if _, err = store.Queries().HeartbeatControlTunnelNode(ctx, dbsqlc.HeartbeatControlTunnelNodeParams{ID: nodeInput.NodeID, ProcessEpoch: registration.ProcessEpoch, Ready: true, Observation: []byte(`{}`), Now: sql.NullTime{Time: time.Now().UTC(), Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled heartbeat accepted: %v", err)
	}
	var state string
	var ready bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT state,ready FROM paperboat.control_tunnel_nodes WHERE id=$1`, nodeInput.NodeID).Scan(&state, &ready); err != nil || state != "draining" || ready {
		t.Fatalf("disabled node=%s/%t err=%v", state, ready, err)
	}
	if _, err = s.MutateNode(ctx, admin, "node-resume-"+x, NodeMutation{Action: "resume", NodeID: nodeInput.NodeID, Reason: "case:test", Revision: page.Items[0].Revision}); !errors.Is(err, ErrConflict) {
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

func TestOperatorNodeSiteIsSetByTheSiteAction(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "infra_admin_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, admin); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, "operator-infrastructure-test-key-32-bytes")
	region, err := s.MutateRegion(ctx, admin, "region-create-"+x, RegionMutation{Action: "create", ID: "region_" + x, DisplayName: "Test region", Reason: "case:test"})
	if err != nil {
		t.Fatal(err)
	}
	in := struct {
		NodeID, EdgePool, EndpointHost string
		EndpointTCPPort                int32
	}{"managed_" + x, "managed:" + region.ID, "edge.example.test", 24443}
	addManagedNode(t, store, "operator-infrastructure-test-key-32-bytes", in.NodeID, region.ID, in.EdgePool, "zone-a", in.EndpointHost, []string{"edge"}, []string{"http3"}, 8)
	site := func() string {
		var v string
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(site,'') FROM paperboat.control_tunnel_nodes WHERE id=$1`, in.NodeID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// A node of a workspace's own pbh has no global installation; the action must not need one.
	if _, err = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE node_id=$1`, in.NodeID); err != nil {
		t.Fatal(err)
	}
	set := NodeMutation{Action: "site", NodeID: in.NodeID, Reason: "case:test", Site: "Mumbai"}
	got, err := s.MutateNode(ctx, admin, "site-"+x, set)
	if err != nil || site() != "Mumbai" {
		t.Fatalf("site action: site=%q err=%v", site(), err)
	}
	// The response is the whole node (OperatorNode), read from the node row alone.
	if n := got.Node; n.ID != in.NodeID || n.RegionID != region.ID || n.EdgePool != in.EdgePool || n.FailureDomain != "zone-a" || n.EndpointHost != in.EndpointHost || n.EndpointTCPPort != in.EndpointTCPPort || n.CapacityLimit != 8 || len(n.Roles) != 1 || n.Roles[0] != "edge" || len(n.Transports) != 1 || n.Transports[0] != "http3" || n.State == "" || n.RegistryExpiresAt.IsZero() || n.Revision != 0 {
		t.Fatalf("site action returned %+v", n)
	}
	if replay, replayErr := s.MutateNode(ctx, admin, "site-"+x, set); replayErr != nil || fmt.Sprint(replay.Node) != fmt.Sprint(got.Node) {
		t.Fatalf("site replay %+v err=%v, want %+v", replay.Node, replayErr, got.Node)
	}
	set.Site = " Mumbai"
	if _, err = s.MutateNode(ctx, admin, "site-bad-"+x, set); !errors.Is(err, ErrInvalid) {
		t.Fatalf("site with surrounding space: %v", err)
	}
	set.Site, set.NodeID = "Mumbai", "no_such_"+x
	if _, err = s.MutateNode(ctx, admin, "site-none-"+x, set); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
}

// A node's forwarding counters come from its latest observation: an edge's "forward", a relay's "mesh"; a node that
// reports none has none, and the same read model serves the list and a mutation's answer.
func TestOperatorNodeListShowsForwardStats(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "infra_admin_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, admin); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, "operator-infrastructure-test-key-32-bytes")
	region, err := s.MutateRegion(ctx, admin, "region-create-"+x, RegionMutation{Action: "create", ID: "region_" + x, DisplayName: "Test region", Reason: "case:test"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, suffix := range []string{"a", "b", "c"} {
		id := "managed_" + suffix + "_" + x
		addManagedNode(t, store, "operator-infrastructure-test-key-32-bytes", id, region.ID, "managed:"+region.ID, "zone-"+suffix, suffix+".example.test", []string{"edge"}, []string{"http3"}, 8)
		ids = append(ids, id)
	}
	set := func(id, observation string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET observation=$2::jsonb WHERE id=$1`, id, observation); err != nil {
			t.Fatal(err)
		}
	}
	set(ids[0], `{"active_streams":1,"forward":{"links":2,"names":9,"forwarded":11,"accepted":5,"holder_miss":1,"unreachable":2,"loop_refused":3,"bytes_in":100,"bytes_out":200}}`)
	set(ids[1], `{"mesh":{"links":1,"names":4,"forwarded":7}}`)
	set(ids[2], `{"active_streams":3}`)
	page, err := s.ListNodes(ctx, admin, region.ID, "", 100)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("list: %+v err=%v", page.Items, err)
	}
	if f := page.Items[0].Forward; f == nil || f.Links != 2 || f.Names != 9 || f.Forwarded != 11 || f.Accepted != 5 || f.HolderMiss != 1 || f.Unreachable != 2 || f.LoopRefused != 3 || f.BytesIn != 100 || f.BytesOut != 200 {
		t.Fatalf("edge forward stats %+v", f)
	}
	if f := page.Items[1].Forward; f == nil || f.Links != 1 || f.Names != 4 || f.Forwarded != 7 {
		t.Fatalf("relay mesh stats %+v", f)
	}
	if page.Items[2].Forward != nil {
		t.Fatalf("a node reporting no counters shows %+v", page.Items[2].Forward)
	}
	raw, err := json.Marshal(page.Items[2])
	if err != nil || strings.Contains(string(raw), `"forward"`) {
		t.Fatalf("a node without counters must omit forward: %s err=%v", raw, err)
	}
	site, err := s.MutateNode(ctx, admin, "site-"+x, NodeMutation{Action: "site", NodeID: ids[0], Reason: "case:test", Site: "Mumbai"})
	if err != nil || site.Node.Forward == nil || site.Node.Forward.Forwarded != 11 {
		t.Fatalf("mutation answer forward %+v err=%v", site.Node.Forward, err)
	}
}
