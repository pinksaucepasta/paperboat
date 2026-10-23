package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Listener metadata is accepted only from the protected, live edge fixture.
// Preview admission, route readiness and ingress authorization remain production APIs.
func task368RegisterBrowserEdge(t *testing.T, ctx context.Context, database *sql.DB, directory, account string) {
	t.Helper()
	path := filepath.Join(directory, "browser-edge-ready.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		t.Fatal("invalid protected browser edge marker")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ready struct {
		Node        string    `json:"node_id"`
		Epoch       string    `json:"process_epoch"`
		HTTP        string    `json:"http_address"`
		TCP         int       `json:"tcp_port"`
		QUIC        int       `json:"quic_port"`
		SPKI        string    `json:"spki"`
		Certificate string    `json:"certificate"`
		Observed    time.Time `json:"observed_at"`
		Ready       bool      `json:"ready"`
	}
	if json.Unmarshal(raw, &ready) != nil {
		t.Fatal("invalid edge marker JSON")
	}
	if !ready.Ready || ready.Observed.Before(time.Now().Add(-5*time.Second)) {
		return
	}
	host, _, err := net.SplitHostPort(ready.HTTP)
	if err != nil || host != "100.70.146.64" || ready.Node != "task368-browser-edge" || len(ready.Epoch) != 32 || ready.TCP < 1 || ready.TCP > 65535 || ready.QUIC < 1 || ready.QUIC > 65535 || ready.Observed.After(time.Now().Add(time.Second)) {
		t.Fatal("unexpected edge listener identity")
	}
	block, _ := pem.Decode([]byte(ready.Certificate))
	if block == nil {
		t.Fatal("edge certificate absent")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if ready.SPKI != "sha256:"+hex.EncodeToString(digest[:]) || cert.VerifyHostname(host) != nil {
		t.Fatal("edge listener certificate mismatch")
	}
	_, err = database.ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,allowed_account_ids) VALUES('task368-browser-edge','task368','v1',$1,'ready',true,$2,'100.70.146.64',$3,$4,$5,$6,ARRAY[$7]::text[]) ON CONFLICT(id) DO UPDATE SET process_epoch=EXCLUDED.process_epoch,state='ready',ready=true,last_heartbeat_at=EXCLUDED.last_heartbeat_at,carrier_endpoint_tcp_port=EXCLUDED.carrier_endpoint_tcp_port,carrier_endpoint_quic_port=EXCLUDED.carrier_endpoint_quic_port,carrier_server_spki_sha256=EXCLUDED.carrier_server_spki_sha256,carrier_server_certificate_chain_pem=EXCLUDED.carrier_server_certificate_chain_pem,allowed_account_ids=EXCLUDED.allowed_account_ids`, ready.Epoch, ready.Observed, ready.TCP, ready.QUIC, ready.SPKI, ready.Certificate, account)
	if err != nil {
		t.Fatal("actual browser edge registration:", err)
	}
}

// Like Task40's connected fixture, provision the helper route only after the
// authenticated target process has bound its real TLS dispatch listener.
func task368RegisterBrowserTarget(t *testing.T, ctx context.Context, database *sql.DB, directory, account string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, "browser-target-ready.json"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || len(raw) > 4096 {
		t.Fatal("invalid browser target marker")
	}
	var ready struct {
		Machine     string    `json:"machine_id"`
		Environment string    `json:"environment_id"`
		Address     string    `json:"http_address"`
		Generation  int64     `json:"installation_generation"`
		Observed    time.Time `json:"observed_at"`
	}
	if json.Unmarshal(raw, &ready) != nil {
		t.Fatal("invalid target JSON")
	}
	if ready.Observed.Before(time.Now().Add(-5 * time.Second)) {
		return
	}
	host, portText, err := net.SplitHostPort(ready.Address)
	port, pe := strconv.Atoi(portText)
	if err != nil || pe != nil || host != "100.70.146.64" || port < 1 || port > 65535 || ready.Generation < 1 || ready.Observed.After(time.Now().Add(time.Second)) {
		t.Fatal("unexpected target listener")
	}
	var valid bool
	err = database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.user_machines WHERE id=$1 AND environment_id=$2 AND user_id=$3 AND installation_generation=$4 AND revoked_at IS NULL AND deleted_at IS NULL)`, ready.Machine, ready.Environment, account, ready.Generation).Scan(&valid)
	if err != nil || !valid {
		t.Fatal("target enrollment binding unavailable", err)
	}
	var edge bool
	if err = database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes WHERE id='task368-browser-edge' AND ready AND last_heartbeat_at>now()-interval '5 seconds')`).Scan(&edge); err != nil {
		t.Fatal(err)
	}
	if !edge {
		return
	}
	_, err = database.ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations(environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES($1,$2,1,'task368','task368-browser-edge','admitted') ON CONFLICT(environment_id,connector_id) DO UPDATE SET machine_id=EXCLUDED.machine_id,edge_pool=EXCLUDED.edge_pool,edge_node_id=EXCLUDED.edge_node_id,state='admitted'`, ready.Environment, ready.Machine)
	if err != nil {
		t.Fatal("helper connector listener registration:", err)
	}
	_, err = database.ExecContext(ctx, `INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) SELECT $1,$2,'runtime_https_wss',$3,'127.0.0.1',$4,1,1,'task368-browser-edge',generation FROM paperboat.control_connector_generations WHERE environment_id=$2 ON CONFLICT(id) DO UPDATE SET public_host=EXCLUDED.public_host,target_port=EXCLUDED.target_port,applied_generation=EXCLUDED.applied_generation`, "task368-browser-"+ready.Machine, ready.Environment, ready.Address, port)
	if err != nil {
		t.Fatal("actual helper route registration:", err)
	}
}

type task368ObservedDispatch struct {
	t    *testing.T
	next interface {
		Dispatch(context.Context, previewv1.DispatchRequest) (previewv1.DispatchOutcome, error)
	}
}

func (d task368ObservedDispatch) Dispatch(ctx context.Context, r previewv1.DispatchRequest) (previewv1.DispatchOutcome, error) {
	d.t.Log("real preview dispatch entered")
	out, e := d.next.Dispatch(ctx, r)
	d.t.Logf("real preview dispatch returned error=%v", e)
	return out, e
}
