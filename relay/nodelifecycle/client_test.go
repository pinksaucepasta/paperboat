package nodelifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"tailscale.com/types/key"
)

func TestNativeRelayLifecycleRevokesConnectedCarrier(t *testing.T) {
	credential := strings.Repeat("test", 16)
	var revoke atomic.Bool
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			http.Error(w, "denied", 401)
			return
		}
		var in struct {
			NodeID             string                      `json:"node_id"`
			ExpectedGeneration uint64                      `json:"expected_generation"`
			Generation         uint64                      `json:"node_generation"`
			Epoch              string                      `json:"process_epoch"`
			Subjects           []derpquic.AuthoritySubject `json:"subjects"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		gen := in.Generation
		if r.URL.Path == "/v1/relay/nodes/start" {
			gen = in.ExpectedGeneration + 1
		}
		out := Lease{NodeID: in.NodeID, Generation: gen, ProcessEpoch: in.Epoch, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256, Revocations: []derpquic.AuthoritySubject{}}
		if revoke.Load() {
			for _, subject := range in.Subjects {
				subject.Generation++
				out.Revocations = append(out.Revocations, subject)
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer api.Close()
	cfg := Config{URL: api.URL, HTTP: api.Client(), Credential: credential, NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")}
	account, endpoint, subjectGeneration := "account", "endpoint", uint64(1)
	if path := os.Getenv("PAPERBOAT_RELAY_LIFECYCLE_FIXTURE"); path != "" {
		var fixture struct {
			URL, Credential, NodeID, AccountID, EndpointID string
			Generation, SubjectGeneration                  uint64
			CA                                             []byte
		}
		raw, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(raw, &fixture) != nil {
			t.Fatal("cannot read lifecycle fixture")
		}
		cert, err := x509.ParseCertificate(fixture.CA)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		cfg.URL, cfg.Credential, cfg.NodeID, cfg.ExpectedGeneration = fixture.URL, fixture.Credential, fixture.NodeID, fixture.Generation
		cfg.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, Timeout: 5 * time.Second}
		defer cfg.HTTP.CloseIdleConnections()
		account, endpoint, subjectGeneration = fixture.AccountID, fixture.EndpointID, fixture.SubjectGeneration
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := c.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: cfg.NodeID, NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch, Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, socket, api.TLS.Clone()) }()
	defer func() { cancel(); server.Close(); <-done; server.Wait() }()
	for !server.Ready() {
		select {
		case <-ctx.Done():
			t.Fatal("relay not ready")
		case <-time.After(time.Millisecond):
		}
	}
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, template, template, clientPublic, clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{clientDER}, PrivateKey: clientPrivate}
	fingerprint := sha256.Sum256(cert.Certificate[0])
	now := time.Now().Unix()
	grant := derpquic.Grant{Version: 1, Issuer: "test", Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 10, Generation: subjectGeneration, AccountID: account, EndpointID: endpoint, WireGuardPublicKey: derpquic.KeyString(key.NewNode().Public()), CertificateFingerprint: hex.EncodeToString(fingerprint[:]), NodeID: cfg.NodeID, NodeGeneration: lease.Generation, ProcessEpoch: lease.ProcessEpoch}
	grant.QUICPublicKey = base64.RawURLEncoding.EncodeToString(clientPublic)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"paperboat-relay-grant+jwt","kid":"test"}`))
	body, _ := json.Marshal(grant)
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(body)
	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(unsigned)))
	tlsConfig := api.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.ServerName = "127.0.0.1"
	tlsConfig.Certificates = []tls.Certificate{cert}
	carrier := derpquic.NewClient(derpquic.ClientConfig{Address: socket.LocalAddr().String(), TLS: tlsConfig, Credential: func(context.Context) (string, error) { return token, nil }})
	defer carrier.Close()
	if err = carrier.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Observe(ctx, server, false); err != nil {
		t.Fatal(err)
	}
	if len(server.AuthoritySubjects()) != 1 {
		t.Fatal("admission metadata absent")
	}
	if err = carrier.Ping(ctx); err != nil {
		t.Fatal("ordinary observation broke carrier")
	}
	revoke.Store(true)
	if cfg.URL != api.URL {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL+"/test/native-relay/revoke", nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Credential)
		resp, err := cfg.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatal("real authority revocation failed")
		}
	}
	if err = c.Observe(ctx, server, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for server.Snapshot().Connections != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.Snapshot().Connections != 0 {
		t.Fatal("revocation did not close carrier")
	}
	replayed := derpquic.NewClient(derpquic.ClientConfig{Address: socket.LocalAddr().String(), TLS: tlsConfig, Credential: func(context.Context) (string, error) { return token, nil }})
	defer replayed.Close()
	if err = replayed.Connect(ctx); err == nil {
		t.Fatal("revoked grant readmitted")
	}
	c.lease.ExpiresAt = time.Now().Unix()
	if err = c.Run(ctx, server); err != ErrControl || !server.Snapshot().Draining {
		t.Fatal("expired lease did not stop relay")
	}
}

func TestControlTraceFailureThenRecovery(t *testing.T) {
	var calls int
	var outcomes []string
	const reference = "pb-0123456789abcdef0123456789abcdef"
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("sentry-trace") == "" || r.Header.Get("Support-Reference") != reference || r.Header.Get("baggage") != "" || r.Header.Get("traceparent") != "" {
			t.Fatalf("trace headers=%v", r.Header)
		}
		if calls == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(Lease{NodeID: "relay", Generation: 2, ProcessEpoch: "epoch", ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 8})
	}))
	defer api.Close()
	client, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state"), ControlTrace: func(_ context.Context, operation string) (string, string, func(string, string)) {
		if operation != "dependency_health" {
			t.Fatalf("operation=%s", operation)
		}
		return "0123456789abcdef0123456789abcdef-0123456789abcdef-1", reference, func(outcome, code string) { outcomes = append(outcomes, outcome+":"+code) }
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.post(context.Background(), "/v1/relay/nodes/observe", map[string]any{}); err != ErrControl {
		t.Fatalf("failure=%v", err)
	}
	if _, err = client.post(context.Background(), "/v1/relay/nodes/observe", map[string]any{}); err != nil {
		t.Fatalf("recovery=%v", err)
	}
	if got := strings.Join(outcomes, ","); got != "failed:unavailable,success:ok" {
		t.Fatalf("outcomes=%s", got)
	}
}

func TestControlCorrelationPropagatesWithoutTrace(t *testing.T) {
	const reference = "pb-fedcba9876543210fedcba9876543210"
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Support-Reference") != reference || r.Header.Get("sentry-trace") != "" {
			t.Fatalf("correlation headers=%v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(Lease{NodeID: "relay", Generation: 2, ProcessEpoch: "epoch", ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 8})
	}))
	defer api.Close()
	client, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state"), ControlTrace: func(context.Context, string) (string, string, func(string, string)) {
		return "", reference, func(string, string) {}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.post(context.Background(), "/v1/relay/nodes/observe", map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRelayStartupStateRetryAndExclusiveOwner(t *testing.T) {
	var calls int
	var epoch string
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NodeID     string `json:"node_id"`
			Generation uint64 `json:"expected_generation"`
			Epoch      string `json:"process_epoch"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		calls++
		if calls == 1 {
			epoch = in.Epoch
			http.Error(w, "lost response", 503)
			return
		}
		if in.Epoch != epoch {
			t.Error("startup retry changed epoch")
		}
		json.NewEncoder(w).Encode(Lease{NodeID: in.NodeID, Generation: in.Generation + 1, ProcessEpoch: in.Epoch, CapacityLimit: 256, ExpiresAt: time.Now().Add(10 * time.Second).Unix(), Revocations: []derpquic.AuthoritySubject{}})
	}))
	defer api.Close()
	cfg := Config{URL: api.URL, HTTP: api.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")}
	first, _ := New(cfg)
	defer first.Close()
	if _, err := first.Start(context.Background()); err == nil {
		t.Fatal("failed control accepted")
	}
	second, _ := New(cfg)
	defer second.Close()
	if _, err := second.Start(context.Background()); err == nil {
		t.Fatal("duplicate process owner accepted")
	}
	first.Close()
	if _, err := second.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRelayControlRejectionFailsImmediately(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fenced", 409) }))
	defer api.Close()
	c, err := New(Config{URL: api.URL, HTTP: api.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1, StatePath: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	c.lease = Lease{NodeID: "relay", Generation: 1, ProcessEpoch: "epoch", ExpiresAt: time.Now().Add(10 * time.Second).Unix(), CapacityLimit: 256}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	server, err := derpquic.NewServer(derpquic.Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = c.Run(ctx, server); err != ErrFenced || !server.Snapshot().Draining {
		t.Fatal("fenced node retained control lease")
	}
}
