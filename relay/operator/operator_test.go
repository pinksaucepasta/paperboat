package operator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRegisterAndSignedAdmin(t *testing.T) {
	var public ed25519.PublicKey
	var installation string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/jwks.json":
			_, _ = w.Write([]byte(`{"keys":[{"kty":"OKP"}]}`))
		case "/v1/selfhost/challenges":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			public, _ = base64.RawURLEncoding.DecodeString(in["public_key"])
			json.NewEncoder(w).Encode(map[string]any{"challenge": "fresh", "expires_at": time.Now().Add(time.Minute).Unix()})
		case "/v1/selfhost/installations":
			var in InstallationRequest
			json.NewDecoder(r.Body).Decode(&in)
			sig, _ := base64.RawURLEncoding.DecodeString(in.Signature)
			in.Signature = ""
			raw, _ := json.Marshal(in)
			if !ed25519.Verify(public, raw, sig) {
				t.Error("invalid registration signature")
			}
			installation = "i_1"
			json.NewEncoder(w).Encode(Registration{InstallationID: installation, NodeID: "n_1", NodeGeneration: 1, RuntimeCredential: "secret"})
		case "/v1/selfhost/installations/i_1/admin":
			if r.Header.Get("X-Paperboat-Operator-Proof") == "" {
				t.Error("missing proof")
			}
			json.NewEncoder(w).Encode(AdminResult{Code: "shareable"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	dir := filepath.Join(t.TempDir(), "state")
	certPath, keyPath := filepath.Join(t.TempDir(), "tls.crt"), filepath.Join(t.TempDir(), "tls.key")
	cert := s.TLS.Certificates[0]
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600)
	_, err := Register(context.Background(), dir, Setup{ControlURL: s.URL, Name: "mine", Capability: "relay", EndpointHost: "127.0.0.1", TCPPort: 443, QUICPort: 444, Region: "eu", FailureDomain: "eu-1", CapacityLimit: 10, TLSCert: certPath, TLSKey: keyPath, ControlCA: certPath}, s.Client())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "runtime.credential"))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	args, _, e := RuntimeArgs([]string{"run", "--state-dir", dir})
	if e != nil || !slices.Contains(args, "-control-ca") {
		t.Fatalf("generated args=%v err=%v", args, e)
	}

	changed := Setup{ControlURL: s.URL, Name: "different", Capability: "relay", EndpointHost: "127.0.0.1", TCPPort: 443, QUICPort: 444, Region: "eu", FailureDomain: "eu-1", CapacityLimit: 10, TLSCert: certPath, TLSKey: keyPath, ControlCA: certPath}
	before, _ := os.ReadFile(filepath.Join(dir, "runtime.args"))
	if _, err := Register(context.Background(), dir, changed, s.Client()); err == nil {
		t.Fatal("registered configuration changed without server authority")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "runtime.args"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed configuration change rewrote runtime")
	}
	out, err := Admin(context.Background(), dir, "create-code", "", nil)
	if err != nil || out.Code != "shareable" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestTunnelSetupExportsDistinctBoundUsageKeyAndRuntimeConfig(t *testing.T) {
	var operatorPublic, usagePublic string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/selfhost/challenges":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			operatorPublic = in["public_key"]
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": "fresh", "expires_at": time.Now().Add(time.Minute).Unix()})
		case "/v1/selfhost/installations":
			var in InstallationRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			usagePublic = in.UsagePublicKey
			_ = json.NewEncoder(w).Encode(Registration{InstallationID: "shi_1", NodeID: "shn_1", NodeGeneration: 1, RuntimeCredential: "runtime", UsageKeyID: "shu_1", EdgePool: "shi_1"})
		case "/.well-known/jwks.json":
			_, _ = w.Write([]byte(`{"keys":[]}`))
		case "/v1/trust/revocations":
			if r.Header.Get("Authorization") != "Bearer runtime" {
				t.Error("missing runtime authorization")
			}
			_, _ = w.Write([]byte(`{"revision":1,"revoked":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	certPath, keyPath := testTLSFiles(t, s)
	dir := filepath.Join(t.TempDir(), "operator")
	_, err := Register(context.Background(), dir, Setup{ControlURL: s.URL, Name: "edge", Capability: "tunnel", EndpointHost: "127.0.0.1", TCPPort: 27443, QUICPort: 27444, Region: "eu", FailureDomain: "eu-1", CapacityLimit: 10, TLSCert: certPath, TLSKey: keyPath, PreviewDomain: "preview.example.test", TunnelDomain: "tunnel.example.test", RuntimeDomain: "runtime.example.test"}, s.Client())
	if err != nil {
		t.Fatal(err)
	}
	if usagePublic == "" || usagePublic == operatorPublic {
		t.Fatal("usage key missing or reused operator key")
	}
	for _, name := range []string{"runtime.credential", "jwks.json", "revocations.json", "usage.key", "deployment.json", "runtime.args"} {
		info, e := os.Stat(filepath.Join(dir, name))
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v mode=%v", name, e, info)
		}
	}
	deploymentRaw, err := os.ReadFile(filepath.Join(dir, "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment map[string]any
	if json.Unmarshal(deploymentRaw, &deployment) != nil || deployment["max_body_bytes"] != float64(50<<20) || deployment["max_header_bytes"] != float64(32<<10) {
		t.Fatalf("generated gateway limits = body %v header %v", deployment["max_body_bytes"], deployment["max_header_bytes"])
	}
	if _, exists := deployment["usage_interval"]; exists {
		t.Fatal("generated strict-v1 deployment retained ignored usage_interval")
	}
	args, handled, e := RuntimeArgs([]string{"run", "--state-dir", dir})
	if e != nil || !handled || len(args) == 0 {
		t.Fatalf("runtime args=%v handled=%v err=%v", args, handled, e)
	}
}

func TestUsageRotationSurvivesRepeatedSetupWithMatchingKeyID(t *testing.T) {
	var rotatedPublic ed25519.PublicKey
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/selfhost/challenges":
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": "fresh", "expires_at": time.Now().Add(time.Minute).Unix()})
		case "/v1/selfhost/installations":
			_ = json.NewEncoder(w).Encode(Registration{InstallationID: "shi_1", NodeID: "shn_1", NodeGeneration: 1, RuntimeCredential: "runtime", UsageKeyID: "shu_1", EdgePool: "shi_1"})
		case "/v1/selfhost/installations/shi_1/admin":
			var in struct {
				UsagePublicKey string `json:"usage_public_key"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			rotatedPublic, _ = base64.RawURLEncoding.Strict().DecodeString(in.UsagePublicKey)
			_ = json.NewEncoder(w).Encode(AdminResult{UsageKeyID: "shu_2"})
		case "/.well-known/jwks.json":
			_, _ = w.Write([]byte(`{"keys":[]}`))
		case "/v1/trust/revocations":
			_, _ = w.Write([]byte(`{"revision":1,"revoked":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	certPath, keyPath := testTLSFiles(t, s)
	setup := Setup{ControlURL: s.URL, Name: "edge", Capability: "tunnel", EndpointHost: "127.0.0.1", TCPPort: 27443, QUICPort: 27444, Region: "eu", FailureDomain: "eu-1", CapacityLimit: 10, TLSCert: certPath, TLSKey: keyPath, PreviewDomain: "preview.example.test", TunnelDomain: "tunnel.example.test", RuntimeDomain: "runtime.example.test"}
	dir := filepath.Join(t.TempDir(), "operator")
	if _, err := Register(t.Context(), dir, setup, s.Client()); err != nil {
		t.Fatal(err)
	}
	if _, err := Admin(t.Context(), dir, "rotate-usage", "", s.Client()); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(t.Context(), dir, setup, s.Client()); err != nil {
		t.Fatal(err)
	}
	state, err := Load(dir)
	if err != nil || state.UsageKeyID != "shu_2" {
		t.Fatalf("operator usage key ID = %q, err=%v", state.UsageKeyID, err)
	}
	var keyDoc struct {
		KeyID      string `json:"key_id"`
		PrivateKey string `json:"private_key"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "usage.key"))
	if err != nil || json.Unmarshal(raw, &keyDoc) != nil || keyDoc.KeyID != "shu_2" {
		t.Fatalf("usage key document ID = %q, err=%v", keyDoc.KeyID, err)
	}
	private, err := base64.RawURLEncoding.Strict().DecodeString(keyDoc.PrivateKey)
	message := []byte("signed usage report")
	if err != nil || len(rotatedPublic) != ed25519.PublicKeySize || !ed25519.Verify(rotatedPublic, message, ed25519.Sign(ed25519.PrivateKey(private), message)) {
		t.Fatal("repeated setup did not retain the rotated signing identity")
	}
}

func TestValidateCapacityMatchesRuntimeCeilings(t *testing.T) {
	relay := Setup{ControlURL: "https://control.example.test", Name: "relay", Capability: "relay", EndpointHost: "relay.example.test", TCPPort: 443, QUICPort: 443, Region: "eu", FailureDomain: "eu-1", CapacityLimit: 257}
	if _, err := validate(relay); err == nil || !strings.Contains(err.Error(), "must not exceed 256") {
		t.Fatalf("relay capacity mismatch accepted: %v", err)
	}
	tunnel := relay
	tunnel.Capability = "tunnel"
	tunnel.TCPPort, tunnel.QUICPort = 27443, 27444
	tunnel.PreviewDomain, tunnel.TunnelDomain, tunnel.RuntimeDomain = "preview.example.test", "tunnel.example.test", "runtime.example.test"
	tunnel.CapacityLimit = 10000
	// Validation proceeds beyond capacity to the deliberately absent test certificate.
	if _, err := validate(tunnel); err == nil || strings.Contains(err.Error(), "capacity") {
		t.Fatalf("tunnel capacity was constrained by relay ceiling: %v", err)
	}
	tunnel.CapacityLimit = 10001
	if _, err := validate(tunnel); err == nil || !strings.Contains(err.Error(), "must not exceed 10000") {
		t.Fatalf("unbounded tunnel capacity accepted: %v", err)
	}
}

func testTLSFiles(t *testing.T, s *httptest.Server) (string, string) {
	t.Helper()
	root := t.TempDir()
	certPath, keyPath := filepath.Join(root, "tls.crt"), filepath.Join(root, "tls.key")
	cert := s.TLS.Certificates[0]
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if e := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); e != nil {
		t.Fatal(e)
	}
	return certPath, keyPath
}

func TestOperatorLockSerializesMutations(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := lockOperator(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockOperator(dir); err == nil {
		second.Close()
		t.Fatal("concurrent administration accepted")
	}
	first.Close()
	next, err := lockOperator(dir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	next.Close()
}

func TestRegisterPreservesInsecureIdentity(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("registration attempted with insecure saved identity")
		w.WriteHeader(500)
	}))
	defer server.Close()
	cert, key := testTLSFiles(t, server)
	dir := t.TempDir()
	path := filepath.Join(dir, "operator.json")
	original := []byte(`{"version":1,"operator_private_key":"existing"}`)
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Register(context.Background(), dir, Setup{ControlURL: server.URL, Name: "relay", Capability: "relay", EndpointHost: "127.0.0.1", TCPPort: 443, QUICPort: 443, Region: "eu", FailureDomain: "eu-1", CapacityLimit: 10, TLSCert: cert, TLSKey: key}, server.Client())
	if err == nil {
		t.Fatal("insecure identity accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("saved operator identity overwritten")
	}
}

func TestOperatorLockRejectsSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockOperator(dir); err == nil {
		lock.Close()
		t.Fatal("shared operator directory accepted")
	}
}
