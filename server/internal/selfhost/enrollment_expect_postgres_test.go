package selfhost

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClaimNode is a server that speaks the real claim protocol over real pinned TLS: it signs its inspection with an
// installation key and its receipt likewise, advertising whatever components the test sets. It stands in for pbh where
// a test needs to choose the region and the roles the machine reports.
type fakeClaimNode struct {
	server        *httptest.Server
	installation  ed25519.PrivateKey
	pin           []byte
	certPEM       string
	secret        string
	name          string
	components    []Component
	inspects      atomic.Int32
	claims        atomic.Int32
	receiptFailed atomic.Bool
}

func newFakeClaimNode(t *testing.T, name string, components ...Component) *fakeClaimNode {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "fake-claim-node"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("93.184.216.35")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &tlsKey.PublicKey, tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	n := &fakeClaimNode{installation: key, pin: sum[:], name: name, components: components, secret: base64.RawURLEncoding.EncodeToString(randomBytes(32)),
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/selfhost/inspect", n.inspect)
	mux.HandleFunc("POST /v1/selfhost/claim", n.claim)
	n.server = httptest.NewUnstartedServer(mux)
	n.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: tlsKey}}, MinVersion: tls.VersionTLS13}
	n.server.StartTLS()
	t.Cleanup(n.server.Close)
	return n
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (n *fakeClaimNode) installationKey() string {
	return base64.RawURLEncoding.EncodeToString(n.installation.Public().(ed25519.PublicKey))
}

// code is the enrollment code the owner pastes.
func (n *fakeClaimNode) code() string {
	return "pbh1." + base64.RawURLEncoding.EncodeToString(n.pin) + "." + n.secret
}

func (n *fakeClaimNode) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+n.secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (n *fakeClaimNode) sign(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(n.installation, b))
}

func (n *fakeClaimNode) inspect(w http.ResponseWriter, r *http.Request) {
	if !n.authorized(w, r) {
		return
	}
	n.inspects.Add(1)
	var in struct {
		Nonce string `json:"nonce"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	inspection := Inspection{Version: 1, Nonce: in.Nonce, InstallationKey: n.installationKey(), Name: n.name, EndpointHost: "93.184.216.35", InfrastructureTLSCertificate: n.certPEM,
		ExpiresAt: time.Now().Add(time.Hour).Unix(), Components: append([]Component(nil), n.components...)}
	inspection.Signature = n.sign(inspection)
	_ = json.NewEncoder(w).Encode(inspection)
}

func (n *fakeClaimNode) claim(w http.ResponseWriter, r *http.Request) {
	if !n.authorized(w, r) {
		return
	}
	n.claims.Add(1)
	var in claimRequest
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	out := receipt{ClaimID: in.ClaimID, ScopeType: in.ScopeType, ScopeID: in.ScopeID, InstallationKey: n.installationKey()}
	for _, c := range in.Components {
		out.Components = append(out.Components, receiptComponent{Capability: c.Capability, InstallationID: c.InstallationID, NodeID: c.NodeID, NodeGeneration: c.NodeGeneration})
	}
	out.Signature = n.sign(out)
	_ = json.NewEncoder(w).Encode(out)
}

func fakeRelay(region string) Component {
	return Component{Capability: "relay", TCPPort: 443, QUICPort: 3478, Region: region, FailureDomain: "rack-a", CapacityLimit: 100}
}

func fakeEdge(region string) Component {
	_, usage, _ := ed25519.GenerateKey(rand.Reader)
	return Component{Capability: "tunnel", TCPPort: 8444, QUICPort: 8445, Region: region, FailureDomain: "rack-a", CapacityLimit: 100,
		UsagePublicKey: base64.RawURLEncoding.EncodeToString(usage.Public().(ed25519.PublicKey))}
}

type enrollExpectFixture struct {
	*drainFixture
	owner string
}

func newEnrollExpectFixture(t *testing.T) *enrollExpectFixture {
	f := newDrainFixture(t)
	f.service.ControlURL = "https://control.example.test"
	f.service.TrustDocuments = func(context.Context) ([]byte, []byte, error) {
		return json.RawMessage(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"claim-test","x":"dGVzdA"}]}`), json.RawMessage(`{"version":1,"revocations":[]}`), nil
	}
	return &enrollExpectFixture{drainFixture: f, owner: f.user("owner")}
}

// enroll claims node for the owner with the given expectations. The address override confines the real connection to
// the fake node's listener.
func (f *enrollExpectFixture) enroll(node *fakeClaimNode, region, capability string) ([]Installation, error) {
	if region != "" {
		// A claim for an expected region needs the region to exist, as the console's Add dialog on a region card does.
		f.exec(`INSERT INTO paperboat.selfhost_regions(scope_kind,scope_id,slug,display_name) VALUES('account',$1,$2,$2) ON CONFLICT DO NOTHING`, f.owner, region)
		f.t.Cleanup(func() { f.exec(`DELETE FROM paperboat.selfhost_regions WHERE scope_id=$1`, f.owner) })
	}
	return f.enrollRaw(node, region, capability)
}

func (f *enrollExpectFixture) enrollRaw(node *fakeClaimNode, region, capability string) ([]Installation, error) {
	f.service.DialAddress = func(context.Context, string) (string, error) { return node.server.Listener.Addr().String(), nil }
	installs, err := f.service.Enroll(f.t.Context(), f.owner, EnrollRequest{Address: node.server.Listener.Addr().String(), Code: node.code(), Scope: Scope{Kind: "account", ID: f.owner}, ExpectRegion: region, ExpectCapability: capability}, false)
	for _, i := range installs {
		f.t.Cleanup(func() {
			f.exec(`DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, i.NodeID)
			f.exec(`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, i.InstallationID)
			f.exec(`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, i.NodeID)
		})
	}
	return installs, err
}

func (f *enrollExpectFixture) installationCount(node *fakeClaimNode) (n int) {
	f.t.Helper()
	key, _ := publicKey(node.installationKey())
	if err := f.store.SQL().QueryRowContext(f.t.Context(), `SELECT count(*) FROM paperboat.selfhost_installations WHERE installation_public_key=$1`, []byte(key)).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *enrollExpectFixture) storedRegion(node string) (region string) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(f.t.Context(), `SELECT region FROM paperboat.control_tunnel_nodes WHERE id=$1`, node).Scan(&region); err != nil {
		f.t.Fatal(err)
	}
	return region
}

func TestEnrollExpectationMatches(t *testing.T) {
	f := newEnrollExpectFixture(t)
	relay := newFakeClaimNode(t, "Home relay", fakeRelay("home-lab"))
	installs, err := f.enroll(relay, "home-lab", "relay")
	if err != nil || len(installs) != 1 || installs[0].Region != "home-lab" || installs[0].Capability != "relay" || installs[0].Draining || installs[0].DrainDeadline != nil {
		t.Fatalf("matching expectation: %+v %v", installs, err)
	}
	// Either expectation alone also matches, and a claim without any is as before.
	edge := newFakeClaimNode(t, "Home edge", fakeEdge("home-lab"))
	if installs, err = f.enroll(edge, "", "tunnel"); err != nil || len(installs) < 1 {
		t.Fatalf("capability only: %v", err)
	}
	other := newFakeClaimNode(t, "Other relay", fakeRelay("other-lab"))
	if _, err = f.enroll(other, "other-lab", ""); err != nil {
		t.Fatalf("region only: %v", err)
	}
	plain := newFakeClaimNode(t, "Plain relay", fakeRelay("plain-lab"))
	if _, err = f.enroll(plain, "", ""); err != nil {
		t.Fatalf("no expectation: %v", err)
	}
}

func TestEnrollExpectationRefusesWrongRegion(t *testing.T) {
	f := newEnrollExpectFixture(t)
	node := newFakeClaimNode(t, "Wrong region", fakeRelay("garage"))
	_, err := f.enroll(node, "home-lab", "relay")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "installed for region garage, not home-lab") || !strings.Contains(err.Error(), "pbh install --region home-lab") {
		t.Fatalf("wrong region: %v", err)
	}
	if f.installationCount(node) != 0 || node.claims.Load() != 0 {
		t.Fatalf("a refused expectation reserved or claimed: installations=%d claims=%d", f.installationCount(node), node.claims.Load())
	}
	// The machine never learns anything: the refusal happens after inspection only.
	if node.inspects.Load() != 1 {
		t.Fatalf("inspects=%d", node.inspects.Load())
	}
}

func TestEnrollExpectationRefusesWrongRole(t *testing.T) {
	f := newEnrollExpectFixture(t)
	edge := newFakeClaimNode(t, "An edge", fakeEdge("home-lab"))
	_, err := f.enroll(edge, "home-lab", "relay")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "installed as an edge, not a relay") || !strings.Contains(err.Error(), "pbh install --relay --region home-lab") {
		t.Fatalf("edge for a relay: %v", err)
	}
	relay := newFakeClaimNode(t, "A relay", fakeRelay("home-lab"))
	_, err = f.enroll(relay, "home-lab", "tunnel")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "installed as a relay, not an edge") || !strings.Contains(err.Error(), "pbh install --edge --region home-lab") {
		t.Fatalf("relay for an edge: %v", err)
	}
	if f.installationCount(edge) != 0 || f.installationCount(relay) != 0 || edge.claims.Load()+relay.claims.Load() != 0 {
		t.Fatal("a refused role reserved or claimed")
	}
}

func TestEnrollExpectationRefusesMachineWithBothRoles(t *testing.T) {
	f := newEnrollExpectFixture(t)
	for _, tc := range []struct{ region, capability string }{{"home-lab", "relay"}, {"home-lab", "tunnel"}, {"home-lab", ""}, {"", "relay"}} {
		both := newFakeClaimNode(t, "Both", fakeRelay("home-lab"), fakeEdge("home-lab"))
		_, err := f.enroll(both, tc.region, tc.capability)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "runs both a relay and an edge") {
			t.Fatalf("both roles with region=%q capability=%q: %v", tc.region, tc.capability, err)
		}
		if f.installationCount(both) != 0 || both.claims.Load() != 0 {
			t.Fatalf("both roles reserved or claimed with %+v", tc)
		}
	}
	// Without an expectation a both-role machine enrolls as it always has.
	both := newFakeClaimNode(t, "Both", fakeRelay("home-lab"), fakeEdge("home-lab"))
	if installs, err := f.enroll(both, "", ""); err != nil || len(installs) != 2 {
		t.Fatalf("both roles without expectation: %d %v", len(installs), err)
	}
}

func TestEnrollExpectationValuesAreValidatedBeforeDialing(t *testing.T) {
	f := newEnrollExpectFixture(t)
	node := newFakeClaimNode(t, "Never dialed", fakeRelay("home-lab"))
	for _, tc := range []struct{ region, capability string }{{"Home Lab", ""}, {"home lab", "relay"}, {"1home", ""}, {strings.Repeat("a", 64), ""}, {"", "ssh"}, {"", "edge"}, {"home-lab", "RELAY"}} {
		if _, err := f.enroll(node, tc.region, tc.capability); !errors.Is(err, ErrInvalid) {
			t.Fatalf("expectation %+v: %v", tc, err)
		}
	}
	if node.inspects.Load() != 0 {
		t.Fatalf("an invalid expectation still dialed the machine %d times", node.inspects.Load())
	}
}

func TestEnrollReuseComparesStoredRegion(t *testing.T) {
	f := newEnrollExpectFixture(t)
	node := newFakeClaimNode(t, "Moving relay", fakeRelay("home-lab"))
	installs, err := f.enroll(node, "home-lab", "relay")
	if err != nil || len(installs) != 1 {
		t.Fatalf("first claim: %v", err)
	}
	// The same machine, same region: a retry of the claim (receipt lost) still succeeds, with or without expectations.
	if again, err := f.enroll(node, "home-lab", "relay"); err != nil || len(again) != 1 || again[0].InstallationID != installs[0].InstallationID {
		t.Fatalf("retry in the same region: %+v %v", again, err)
	}
	if again, err := f.enroll(node, "", ""); err != nil || len(again) != 1 {
		t.Fatalf("retry without expectation: %v", err)
	}
	// The machine now reports another region (reinstalled with --region garage): the stored one is what pairs name and
	// the owner sees, so reuse refuses with and without an expectation, and the stored region is unchanged.
	node.components = []Component{fakeRelay("garage")}
	for _, region := range []string{"garage", ""} {
		_, err = f.enroll(node, region, "relay")
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "already added in region home-lab but now reports region garage") {
			t.Fatalf("reuse with another region (expect %q): %v", region, err)
		}
	}
	// An expectation of the old region fails on the machine's reported region first.
	if _, err = f.enroll(node, "home-lab", "relay"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "installed for region garage, not home-lab") {
		t.Fatalf("expectation of the stored region against a machine that reports another: %v", err)
	}
	if got := f.storedRegion(installs[0].NodeID); got != "home-lab" {
		t.Fatalf("stored region changed to %q", got)
	}
	if f.installationCount(node) != 1 {
		t.Fatalf("installations=%d", f.installationCount(node))
	}
}
