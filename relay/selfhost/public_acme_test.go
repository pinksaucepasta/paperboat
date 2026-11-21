package selfhost

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeACMECA is the smallest RFC 8555 server the x/crypto/acme client completes an order against: it checks the
// HTTP-01 answer by fetching it from the node's port 80 (challengeURL, served from the node's challenge directory)
// and signs the CSR it is given with its own CA. JWS signatures are not verified: the client is the system under test.
type fakeACMECA struct {
	*httptest.Server
	challengeURL string
	ca           *x509.Certificate
	caKey        *ecdsa.PrivateKey
	mu           sync.Mutex
	nonce        int
	token        string
	identifier   map[string]string
	profile      string
	validated    bool
	issued       int
	csr          *x509.CertificateRequest
}

func newFakeACMECA(t *testing.T, challengeURL string) *fakeACMECA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake ACME CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	f := &fakeACMECA{challengeURL: challengeURL, ca: ca, caKey: caKey, token: "tok" + strings.Repeat("x", 30)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeACMECA) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := f.URL
	f.nonce++
	w.Header().Set("Replay-Nonce", "nonce"+string(rune('a'+f.nonce%26))+strings.Repeat("n", f.nonce))
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var payload []byte
	if r.Method == http.MethodPost {
		var jws struct{ Payload string }
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &jws)
		payload, _ = base64.RawURLEncoding.DecodeString(jws.Payload)
	}
	order := func(status string) map[string]any {
		o := map[string]any{"status": status, "identifiers": []any{f.identifier}, "authorizations": []string{base + "/authz/1"}, "finalize": base + "/finalize/1"}
		if f.issued > 0 && status == "valid" {
			o["certificate"] = base + "/cert/1"
		}
		return o
	}
	switch r.URL.Path {
	case "/directory":
		reply(200, map[string]any{"newNonce": base + "/nonce", "newAccount": base + "/acct", "newOrder": base + "/order", "meta": map[string]any{"profiles": map[string]string{"shortlived": "profile"}}})
	case "/nonce":
		w.WriteHeader(http.StatusNoContent)
	case "/acct":
		w.Header().Set("Location", base+"/acct/1")
		reply(201, map[string]any{"status": "valid"})
	case "/order":
		var request struct {
			Identifiers []map[string]string
			Profile     string
		}
		_ = json.Unmarshal(payload, &request)
		f.identifier, f.profile, f.validated = request.Identifiers[0], request.Profile, false
		w.Header().Set("Location", base+"/order/1")
		reply(201, order("pending"))
	case "/authz/1":
		status := "pending"
		if f.validated {
			status = "valid"
		}
		reply(200, map[string]any{"status": status, "identifier": f.identifier, "challenges": []any{map[string]any{"type": "http-01", "url": base + "/chal/1", "token": f.token, "status": status}}})
	case "/chal/1":
		answer, err := http.Get(f.challengeURL + "/.well-known/acme-challenge/" + f.token)
		if err == nil {
			body, _ := io.ReadAll(answer.Body)
			answer.Body.Close()
			f.validated = answer.StatusCode == 200 && strings.HasPrefix(string(body), f.token+".")
		}
		reply(200, map[string]any{"type": "http-01", "url": base + "/chal/1", "token": f.token, "status": "processing"})
	case "/order/1":
		if f.issued > 0 {
			reply(200, order("valid"))
		} else if f.validated {
			reply(200, order("ready"))
		} else {
			reply(200, order("pending"))
		}
	case "/finalize/1":
		var request struct{ CSR string }
		_ = json.Unmarshal(payload, &request)
		der, _ := base64.RawURLEncoding.DecodeString(request.CSR)
		f.csr, _ = x509.ParseCertificateRequest(der)
		f.issued++
		reply(200, order("valid"))
	case "/cert/1":
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(100 + f.issued)), DNSNames: f.csr.DNSNames, IPAddresses: f.csr.IPAddresses, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(6 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, leaf, f.ca, f.csr.PublicKey, f.caKey)
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw})...))
	default:
		http.NotFound(w, r)
	}
}

// automaticNode is an installation state directory of a Paperboat-managed node with automatic browser HTTPS on.
func automaticNode(t *testing.T, host string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{Name: "managed", EndpointHost: host, Listen: "127.0.0.1:7277", Components: []Component{{Capability: "tunnel", TCPPort: 7272, QUICPort: 7273, Region: "test", FailureDomain: "test", CapacityLimit: 1}}}
	if err := Initialize(dir, c); err != nil {
		t.Fatal(err)
	}
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.RuntimeReady, s.ClaimHash, s.Claim = true, "claim", &ClaimRequest{ScopeType: "global"}
	s.Config.PublicHTTPSAutomatic = true
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRenewPublicHTTPSObtainsThenRenewsAtTwoThirdsOfLifetime(t *testing.T) {
	for _, host := range []string{"edge.example.test", "203.0.113.7"} {
		t.Run(host, func(t *testing.T) {
			dir := automaticNode(t, host)
			if err := os.MkdirAll(PublicACMEChallengeDir(dir), 0700); err != nil {
				t.Fatal(err)
			}
			edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.StripPrefix("/.well-known/acme-challenge/", http.FileServer(http.Dir(PublicACMEChallengeDir(dir)))).ServeHTTP(w, r)
			}))
			defer edge.Close()
			ca := newFakeACMECA(t, edge.URL)
			now := time.Now()

			renewed, err := RenewPublicHTTPS(t.Context(), dir, ca.URL+"/directory", now)
			if err != nil || !renewed {
				t.Fatalf("first issuance = %v, %v; want a new certificate", renewed, err)
			}
			if ca.profile != "shortlived" || (ca.identifier["value"] != host) || ca.identifier["type"] != map[bool]string{true: "ip", false: "dns"}[host == "203.0.113.7"] {
				t.Fatalf("order = identifier %v profile %q, want %s under the short-lived profile", ca.identifier, ca.profile, host)
			}
			s, err := load(dir)
			if err != nil || s.Config.PublicHTTPSCertFile != publicACMEFile(dir, "cert.pem") || s.Config.PublicHTTPSKeyFile != publicACMEFile(dir, "key.pem") {
				t.Fatalf("state paths = %q %q (%v), want the node's own certificate", s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, err)
			}
			if err := validatePublicHTTPS(host, s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile, now); err != nil {
				t.Fatalf("issued certificate is not accepted at start: %v", err)
			}
			if leftover, _ := os.ReadDir(PublicACMEChallengeDir(dir)); len(leftover) != 0 {
				t.Fatalf("challenge answers left behind: %v", leftover)
			}
			status, err := ReadPublicHTTPSStatus(dir)
			if err != nil || !status.Automatic || status.LastError != "" || status.NotAfter.IsZero() || !strings.Contains(status.Describe(), "valid until") {
				t.Fatalf("status = %+v (%v)", status, err)
			}

			// Before two thirds of the 6-day lifetime has passed nothing is asked of the CA.
			if renewed, err := RenewPublicHTTPS(t.Context(), dir, ca.URL+"/directory", now.Add(3*24*time.Hour)); renewed || err != nil || ca.issued != 1 {
				t.Fatalf("renewal at half life = %v, %v, issued %d; want none", renewed, err, ca.issued)
			}
			// After it, a new certificate replaces the old one.
			if renewed, err := RenewPublicHTTPS(t.Context(), dir, ca.URL+"/directory", now.Add(5*24*time.Hour)); !renewed || err != nil || ca.issued != 2 {
				t.Fatalf("renewal past two thirds = %v, %v, issued %d; want a second certificate", renewed, err, ca.issued)
			}
		})
	}
}

func TestRenewPublicHTTPSFailureIsRecordedAndKeepsTheCurrentCertificate(t *testing.T) {
	dir := automaticNode(t, "edge.example.test")
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // a CA that cannot be reached
	now := time.Now()
	for attempt := range 2 {
		if renewed, err := RenewPublicHTTPS(t.Context(), dir, dead.URL+"/directory", now.Add(time.Duration(attempt)*time.Hour)); renewed || err == nil {
			t.Fatalf("attempt %d = %v, %v; want an error and no certificate", attempt, renewed, err)
		}
	}
	status, err := ReadPublicHTTPSStatus(dir)
	if err != nil || status.LastError == "" || !status.FailingSince.Equal(now.UTC().Truncate(time.Second)) && status.FailingSince.Sub(now) > time.Second {
		t.Fatalf("status = %+v (%v), want the error with FailingSince at the first failure", status, err)
	}
	if got := status.Describe(); !strings.Contains(got, "FAILING since") {
		t.Fatalf("Describe = %q", got)
	}
}

func TestRenewPublicHTTPSDoesNothingUnlessAutomatic(t *testing.T) {
	dir := automaticNode(t, "edge.example.test")
	s, _ := load(dir)
	s.Config.PublicHTTPSAutomatic = false
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	if renewed, err := RenewPublicHTTPS(t.Context(), dir, "http://127.0.0.1:1/directory", time.Now()); renewed || err != nil {
		t.Fatalf("not automatic = %v, %v; want nothing", renewed, err)
	}
	if status, err := ReadPublicHTTPSStatus(dir); err != nil || status.Automatic || status.Describe() != "" {
		t.Fatalf("status for a node without automatic HTTPS = %+v (%v)", status, err)
	}
}

// An automatic certificate that expired while the node was down must not keep the runtime from starting: the
// deployment drops the certificate (the node serves its enrolled identity) and tells the edge where to answer ACME.
func TestSyncPublicHTTPSDeploymentStartsWithAnExpiredAutomaticCertificate(t *testing.T) {
	dir := automaticNode(t, "edge.example.test")
	s, _ := load(dir)
	s.Config.PublicHTTPSCertFile, s.Config.PublicHTTPSKeyFile = publicACMEFile(dir, "cert.pem"), publicACMEFile(dir, "key.pem")
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Config.PublicHTTPSCertFile), 0700); err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"edge.example.test"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	if err := writeFile(s.Config.PublicHTTPSCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(s.Config.PublicHTTPSKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(dir, "tunnel")
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(runtimeDir, "deployment.json"), map[string]any{"public_https_cert_file": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := syncPublicHTTPSDeployment(dir); err != nil {
		t.Fatalf("an expired automatic certificate stopped the runtime from starting: %v", err)
	}
	var deployment map[string]string
	raw, _ := os.ReadFile(filepath.Join(runtimeDir, "deployment.json"))
	if err := json.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment["public_https_cert_file"] != "" || deployment["public_acme_challenge_dir"] != PublicACMEChallengeDir(dir) {
		t.Fatalf("deployment = %v, want no certificate and the challenge directory", deployment)
	}
}
