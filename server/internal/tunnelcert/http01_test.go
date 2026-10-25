package tunnelcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

const http01TestToken = "tok_http01-fixture_0123456789"

func TestHTTP01RegistryServesOnlyTargetedEdgeAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	registry := NewHTTP01Responses(clock)
	record := HTTP01Record{Hostname: "app.example.test", Token: http01TestToken, KeyAuthorization: http01TestToken + ".thumb", EdgeNodeIDs: []string{"edge_a"}}
	if err := registry.Publish(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if got := registry.ForEdge("edge_b"); len(got) != 0 {
		t.Fatalf("edge that does not serve the domain received a response: %v", got)
	}
	got := registry.ForEdge("edge_a")
	if len(got) != 1 || got[0].Hostname != "app.example.test" || got[0].KeyAuthorization != record.KeyAuthorization {
		t.Fatalf("targeted edge did not receive the response: %+v", got)
	}
	if err := registry.Wait(context.Background(), record); err != nil {
		t.Fatalf("fetched response did not satisfy Wait: %v", err)
	}
	now = now.Add(http01ResponseLifetime)
	if got := registry.ForEdge("edge_a"); len(got) != 0 {
		t.Fatalf("expired response still served: %v", got)
	}
	if err := registry.Wait(context.Background(), record); !errors.Is(err, ErrDNSChallengeUnavailable) {
		t.Fatalf("expired response did not withdraw: %v", err)
	}
}

func TestHTTP01RegistryRejectsUnsafeRecords(t *testing.T) {
	registry := NewHTTP01Responses(time.Now)
	valid := HTTP01Record{Hostname: "app.example.test", Token: http01TestToken, KeyAuthorization: http01TestToken + ".thumb", EdgeNodeIDs: []string{"edge_a"}}
	for name, mutate := range map[string]func(*HTTP01Record){
		"wildcard":          func(r *HTTP01Record) { r.Hostname = "*.example.test" },
		"uppercase_host":    func(r *HTTP01Record) { r.Hostname = "App.example.test" },
		"token_with_slash":  func(r *HTTP01Record) { r.Token = "a/b"; r.KeyAuthorization = "a/b.thumb" },
		"foreign_key_auth":  func(r *HTTP01Record) { r.KeyAuthorization = "other.thumb" },
		"no_edge":           func(r *HTTP01Record) { r.EdgeNodeIDs = nil },
		"oversized_keyauth": func(r *HTTP01Record) { r.KeyAuthorization = http01TestToken + "." + strings.Repeat("x", 600) },
	} {
		t.Run(name, func(t *testing.T) {
			record := valid
			mutate(&record)
			if err := registry.Publish(context.Background(), record); !errors.Is(err, ErrDNSChallengeUnavailable) {
				t.Fatalf("unsafe record accepted: %v", err)
			}
		})
	}
}

func TestHTTP01RegistryWaitIsBoundedByContext(t *testing.T) {
	registry := NewHTTP01Responses(time.Now)
	record := HTTP01Record{Hostname: "app.example.test", Token: http01TestToken, KeyAuthorization: http01TestToken + ".thumb", EdgeNodeIDs: []string{"edge_a"}}
	if err := registry.Publish(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := registry.Wait(ctx, record); !errors.Is(err, ErrDNSChallengePending) {
		t.Fatalf("unfetched response reported as ready: %v", err)
	}
	if err := registry.Withdraw(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if got := registry.ForEdge("edge_a"); len(got) != 0 {
		t.Fatalf("withdrawn response still served: %v", got)
	}
}

func TestNewACMEIssuerRequiresExactlyOneChallengeProvider(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := ACMEIssuerConfig{DirectoryURL: "https://acme.example.test/directory", AccountKID: "https://acme.example.test/acct/1", AccountKey: key}
	if _, err := NewACMEIssuer(base); !errors.Is(err, ErrDNSChallengeUnavailable) {
		t.Fatalf("issuer without a challenge provider accepted: %v", err)
	}
	both := base
	both.DNS = deadlineUnusedDNS{}
	both.HTTP01 = NewHTTP01Responses(time.Now)
	if _, err := NewACMEIssuer(both); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issuer with two challenge providers accepted: %v", err)
	}
}

// TestACMEIssuerHTTP01IssuesThroughEdgeAndWithdraws runs the full RFC 8555
// order against a fake CA. The CA accepts the http-01 challenge only when the
// simulated edge has been handed the exact key authorization, and it signs a
// real certificate. The issued bundle and an empty registry are the evidence.
func TestACMEIssuerHTTP01IssuesThroughEdgeAndWithdraws(t *testing.T) {
	ca, caKey := newHTTP01TestCA(t)
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewHTTP01Responses(time.Now)
	fake := newHTTP01FakeCA(ca, caKey, accountKey, "app.example.test")
	server := httptest.NewServer(fake)
	defer server.Close()
	fake.base = server.URL
	issuer, err := NewACMEIssuer(ACMEIssuerConfig{
		DirectoryURL: server.URL + "/directory", AccountKID: server.URL + "/acct/1", AccountKey: accountKey,
		HTTP01: registry, HTTPClient: server.Client(), Timeout: 30 * time.Second,
		PropagationTimeout: 10 * time.Second, CleanupTimeout: 5 * time.Second, PollInterval: 10 * time.Millisecond,
		ChallengeZone: "challenge.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	edge := runHTTP01SimulatedEdge(ctx, registry, "edge_b")
	fake.edge = edge
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.EdgeTargets = []EdgeTarget{{NodeID: "edge_b", ProcessEpoch: "epoch-0002", Generation: 9}}
	bundle, err := issuer.IssueDNS01(ctx, domain)
	if err != nil {
		t.Fatalf("http-01 issuance failed: %v (fake CA: %s)", err, fake.failure())
	}
	if !edge.served(http01TestToken, fake.expectedKeyAuthorization()) {
		t.Fatal("simulated edge never served the exact key authorization")
	}
	leaf, err := parseACMECertificate(bundle.CertificatePEM)
	if err != nil {
		t.Fatalf("issued leaf is invalid: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "app.example.test" {
		t.Fatalf("issued names = %v", leaf.DNSNames)
	}
	if got := registry.ForEdge("edge_b"); len(got) != 0 {
		t.Fatalf("http-01 response was not withdrawn after issuance: %v", got)
	}
}

func TestACMEIssuerHTTP01FailsWhenEdgeNeverFetches(t *testing.T) {
	ca, caKey := newHTTP01TestCA(t)
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewHTTP01Responses(time.Now)
	fake := newHTTP01FakeCA(ca, caKey, accountKey, "app.example.test")
	server := httptest.NewServer(fake)
	defer server.Close()
	fake.base = server.URL
	issuer, err := NewACMEIssuer(ACMEIssuerConfig{
		DirectoryURL: server.URL + "/directory", AccountKID: server.URL + "/acct/1", AccountKey: accountKey,
		HTTP01: registry, HTTPClient: server.Client(), Timeout: 30 * time.Second,
		PropagationTimeout: 300 * time.Millisecond, CleanupTimeout: 5 * time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	domain := testDomain("app.example.test", StrategyDelegatedDNS01)
	domain.EdgeTargets = []EdgeTarget{{NodeID: "edge_b", ProcessEpoch: "epoch-0002", Generation: 9}}
	if _, err := issuer.IssueDNS01(context.Background(), domain); !errors.Is(err, ErrDNSChallengePending) {
		t.Fatalf("unfetched http-01 response did not fail as pending: %v", err)
	}
	if got := registry.ForEdge("edge_b"); len(got) != 0 {
		t.Fatalf("unfetched http-01 response was not withdrawn: %v", got)
	}
}

type http01SimulatedEdge struct {
	mu       sync.Mutex
	response map[string]string
}

func (e *http01SimulatedEdge) served(token, keyAuthorization string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.response[token] == keyAuthorization
}

// runHTTP01SimulatedEdge fetches the registry as an edge would on its ingress
// refresh and remembers what it would serve at /.well-known/acme-challenge/.
func runHTTP01SimulatedEdge(ctx context.Context, registry *HTTP01Responses, nodeID string) *http01SimulatedEdge {
	edge := &http01SimulatedEdge{response: make(map[string]string)}
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			for _, response := range registry.ForEdge(nodeID) {
				edge.mu.Lock()
				edge.response[response.Token] = response.KeyAuthorization
				edge.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return edge
}

func newHTTP01TestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "http-01 fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

// http01FakeCA is a minimal RFC 8555 authority for one identifier. It answers
// http-01 only after the simulated edge has been handed the response.
type http01FakeCA struct {
	base       string
	ca         *x509.Certificate
	caKey      *ecdsa.PrivateKey
	accountKey *ecdsa.PrivateKey
	identifier string
	edge       *http01SimulatedEdge

	mu         sync.Mutex
	verified   bool
	finalized  bool
	failureMsg string
	certPEM    []byte
}

func newHTTP01FakeCA(ca *x509.Certificate, caKey, accountKey *ecdsa.PrivateKey, identifier string) *http01FakeCA {
	return &http01FakeCA{ca: ca, caKey: caKey, accountKey: accountKey, identifier: identifier}
}

func (f *http01FakeCA) expectedKeyAuthorization() string {
	keyAuthorization, _ := (&acme.Client{Key: f.accountKey}).HTTP01ChallengeResponse(http01TestToken)
	return keyAuthorization
}

func (f *http01FakeCA) failure() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failureMsg
}

func (f *http01FakeCA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", "http01-fixture-nonce")
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	authz := f.base + "/authz/1"
	switch r.URL.Path {
	case "/directory":
		_ = json.NewEncoder(w).Encode(map[string]string{"newNonce": f.base + "/nonce", "newOrder": f.base + "/new-order"})
	case "/nonce":
		w.WriteHeader(http.StatusOK)
	case "/new-order":
		w.Header().Set("Location", f.base+"/order/1")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending", "authorizations": []string{authz}, "finalize": f.base + "/finalize/1"})
	case "/authz/1":
		status := "pending"
		if f.verified {
			status = "valid"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": status, "identifier": map[string]string{"type": "dns", "value": f.identifier},
			"challenges": []map[string]any{{"type": "http-01", "status": "pending", "url": f.base + "/chal/1", "token": http01TestToken}},
		})
	case "/chal/1":
		f.acceptHTTP01(w)
	case "/order/1":
		status := "pending"
		if f.verified {
			status = "ready"
		}
		if f.finalized {
			status = "valid"
		}
		order := map[string]any{"status": status, "authorizations": []string{authz}, "finalize": f.base + "/finalize/1"}
		if f.finalized {
			order["certificate"] = f.base + "/cert/1"
		}
		_ = json.NewEncoder(w).Encode(order)
	case "/finalize/1":
		f.finalize(w, r)
	case "/cert/1":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = w.Write(f.certPEM)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// acceptHTTP01 is the CA's validation step: it succeeds only when the edge
// fetched the exact key authorization for the identifier.
func (f *http01FakeCA) acceptHTTP01(w http.ResponseWriter) {
	if !f.edge.served(http01TestToken, f.expectedKeyAuthorization()) {
		f.failureMsg = "edge did not serve the key authorization"
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.verified = true
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "http-01", "status": "valid", "token": http01TestToken, "url": f.base + "/chal/1"})
}

// finalize signs the CSR from the JWS payload with the fake CA.
func (f *http01FakeCA) finalize(w http.ResponseWriter, r *http.Request) {
	var envelope struct {
		Payload string `json:"payload"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &envelope) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	var request struct {
		CSR string `json:"csr"`
	}
	if err != nil || json.Unmarshal(payload, &request) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	csrDER, err := base64.RawURLEncoding.DecodeString(request.CSR)
	csr, parseErr := x509.ParseCertificateRequest(csrDER)
	if err != nil || parseErr != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: f.identifier}, DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, f.ca, csr.PublicKey, f.caKey)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.certPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw})...)
	f.finalized = true
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "valid", "authorizations": []string{f.base + "/authz/1"}, "finalize": f.base + "/finalize/1", "certificate": f.base + "/cert/1"})
}
