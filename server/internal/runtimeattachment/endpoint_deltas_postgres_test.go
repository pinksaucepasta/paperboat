package runtimeattachment

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
)

// Lane W scale matrix: an edge with endpoint deltas gets its endpoint routes only from the endpoint
// pull, as a full set once and then only what changed since its cursor; an idle poll carries no
// admission, a stopped preview arrives as a removed route, a new one as an admission.
func TestEndpointAdmissionDeltasOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "epdelta")
	ctx := f.ctx
	register := func(lease string) {
		t.Helper()
		host := strings.ReplaceAll(lease, "_", "-") + ".preview.example.test"
		f.exec(t, `INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline)
VALUES($1,$2,$3,$3,$4,'owner_session_delta','foreground','http','127.0.0.1:3000','public',now()+interval '10 minutes')`, lease, "https://"+host, f.account, f.machine)
		if err := f.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			return endpoints.RegisterPreviewTx(ctx, tx, endpoints.Preview{LeaseID: lease, AccountID: f.account, AgentMachineID: f.machine, AccessMode: "public", Endpoint: "https://" + host})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Real edge carrier certificates, so the edge's own validation can read the golden answer below.
	for node, host := range map[string]string{f.node: "edge.example.test", f.backupNode: "backup.example.test"} {
		chain, pin := testEdgeCertificate(t, host)
		f.exec(t, `UPDATE control_tunnel_nodes SET carrier_server_certificate_chain_pem=$2,carrier_server_spki_sha256=$3 WHERE id=$1`, node, chain, pin)
	}
	first, second := f.prefix+"_first", f.prefix+"_second"
	register(first)
	deltas := []string{runtimewire.CapabilityEndpoints, runtimewire.CapabilityEndpointDeltas}
	if _, err := f.service.Pull(ctx, f.edge, deltas); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Pull(ctx, f.backupEdge, deltas); err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.Capabilities = []string{runtimewire.CapabilityEndpoints}
	if a, err := f.service.Attach(ctx, f.proof, request); err != nil || !a.EndpointsServed {
		t.Fatalf("attach: served=%v %v", a.EndpointsServed, err)
	}
	// The full runtime pull of a delta edge leaves the endpoint routes out.
	if values, err := f.service.Pull(ctx, f.edge, deltas); err != nil || len(values) != 1 || values[0].EndpointID != "" {
		t.Fatalf("full runtime pull of a delta edge = %d admissions, %v", len(values), err)
	}
	full, err := f.service.PullEndpoints(ctx, f.edge, deltas, "", "")
	if err != nil || !full.Complete || len(flatten(full)) != 1 || flatten(full)[0].EndpointID != first || full.Cursor == "" || full.Admissions != nil {
		t.Fatalf("full endpoint pull = %+v, %v", full, err)
	}
	// Nothing changed since the cursor (rows older than the overlap): an empty delta.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	idle, err := f.service.PullEndpoints(ctx, f.edge, deltas, full.Cursor, "")
	if err != nil || idle.Complete || idle.Carriers == nil || len(flatten(idle)) != 0 || len(idle.RemovedRouteIDs) != 0 || idle.ExpiresAt == nil {
		t.Fatalf("idle delta = %+v, %v", idle, err)
	}
	// A new preview arrives as an admission, a stopped one as a removed route.
	register(second)
	f.exec(t, `UPDATE preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, first)
	changed, err := f.service.PullEndpoints(ctx, f.edge, deltas, idle.Cursor, "")
	if err != nil || changed.Complete || len(flatten(changed)) != 1 || flatten(changed)[0].EndpointID != second ||
		len(changed.RemovedRouteIDs) != 1 || changed.RemovedRouteIDs[0] != first {
		t.Fatalf("delta after a change = %+v, %v", changed, err)
	}
	// An access change arrives as the updated admission.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	f.exec(t, `UPDATE endpoints SET access_mode='private' WHERE id=$1`, second)
	access, err := f.service.PullEndpoints(ctx, f.edge, deltas, changed.Cursor, "")
	if err != nil || len(flatten(access)) != 1 || flatten(access)[0].RouteKind != runtimewire.EndpointRestrictedRouteKind {
		t.Fatalf("delta after an access change = %+v, %v", access, err)
	}
	writeEndpointPullGolden(t, full, changed)
	// A carrier renewal (only expires_at inside its admission) is not a change.
	f.exec(t, `UPDATE endpoint_edge_assignments SET changed_at=now()-interval '1 minute' WHERE edge_node_id IN ($1,$2)`, f.node, f.backupNode)
	f.exec(t, `UPDATE runtime_carrier_attachments SET admission=jsonb_set(admission,'{expires_at}',to_jsonb(to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'))) WHERE machine_id=$1`, f.machine)
	renewed, err := f.service.PullEndpoints(ctx, f.edge, deltas, changed.Cursor, "")
	if err != nil || len(flatten(renewed)) != 0 || len(renewed.RemovedRouteIDs) != 0 {
		t.Fatalf("a renewal produced a delta: %+v, %v", renewed, err)
	}
	// A full answer larger than a page comes in pages that together hold every route once.
	register(f.prefix + "_third")
	defer func(rows int) { endpointPageRows = rows }(endpointPageRows)
	endpointPageRows = 1
	seen, page := map[string]bool{}, ""
	for i := 0; ; i++ {
		answer, err := f.service.PullEndpoints(ctx, f.edge, deltas, "", page)
		if err != nil || !answer.Complete || i > 5 {
			t.Fatalf("paged full answer: %+v %v", answer, err)
		}
		for _, a := range flatten(answer) {
			if seen[a.EndpointID] {
				t.Fatalf("route %s on two pages", a.EndpointID)
			}
			seen[a.EndpointID] = true
		}
		if page = answer.Next; page == "" {
			break
		}
	}
	if len(seen) != 2 || !seen[second] {
		t.Fatalf("paged full answer held %v", seen)
	}
	endpointPageRows = 500
	// An old or missing cursor gets a full answer.
	if again, err := f.service.PullEndpoints(ctx, f.edge, deltas, "not-a-cursor", ""); err != nil || !again.Complete {
		t.Fatalf("invalid cursor = %+v, %v", again, err)
	}
}

// An edge without endpoint deltas decodes strictly: its answer has no cursor, removed list or
// expiry, exactly as before.
func TestEndpointPullWithoutDeltasKeepsItsShapeOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "epnodelta")
	snapshot, err := f.service.PullEndpoints(f.ctx, f.edge, []string{runtimewire.CapabilityEndpoints}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil || string(raw) != `{"schema":"paperboat.runtime-carrier/v1","complete":true,"admissions":[]}` {
		t.Fatalf("answer to an edge without deltas = %s, %v", raw, err)
	}
}

// flatten expands a grouped endpoint answer into its admissions.
func flatten(s runtimewire.Snapshot) []runtimewire.Admission {
	var out []runtimewire.Admission
	if s.Carriers != nil {
		for _, c := range *s.Carriers {
			out = append(out, c.Admissions()...)
		}
	}
	return out
}

// endpointPullGolden holds a full and a delta endpoint answer as the server really sends them, read
// by the edge's client in tunnel/internal/control (TestEndpointPullGoldenThroughTheEdgeClient).
// PAPERBOAT_UPDATE_GOLDEN=<shared/runtimeattachment/testdata> rewrites it.
func writeEndpointPullGolden(t *testing.T, full, delta runtimewire.Snapshot) {
	t.Helper()
	dir := os.Getenv("PAPERBOAT_UPDATE_GOLDEN")
	if dir == "" {
		return
	}
	encoded, err := json.MarshalIndent(map[string]any{"full": full, "delta": delta}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "endpoint_pull.json"), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testEdgeCertificate is a self-signed edge carrier certificate for host, valid for ten years, and
// its pin (sha256 of the leaf's public key info, hex).
func testEdgeCertificate(t *testing.T, host string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "sha256:" + hex.EncodeToString(digest[:])
}
