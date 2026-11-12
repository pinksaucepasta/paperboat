package connectorprotocol

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testCertificatePEM(t *testing.T, isCA bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: isCA, BasicConstraintsValid: true}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}))
}

// A route trusts only real CA certificates in PEM, bounded; a leaf, a key, a path or trailing text is refused.
func TestValidateOriginCA(t *testing.T) {
	root := testCertificatePEM(t, true)
	for name, test := range map[string]struct {
		value string
		ok    bool
	}{
		"one CA":              {root, true},
		"root + intermediate": {root + testCertificatePEM(t, true), true},
		"leaf, not a CA":      {testCertificatePEM(t, false), false},
		"a path":              {"/data/caddy/pki/authorities/local/root.crt", false},
		"trailing text":       {root + "junk", false},
		"leading text":        {"junk\n" + root, false},
		"text between":        {root + "junk\n" + root, false},
		"blank lines between": {root + "\n\n" + root, true},
		"a private key":       {root + "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", false},
		"nine certificates":   {strings.Repeat(root, 9), false},
		"empty":               {"", false},
	} {
		if err := ValidateOriginCA(test.value); (err == nil) != test.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, test.ok)
		}
	}
	if OriginCAFingerprint(root) == "" || OriginCAFingerprint(root) == OriginCAFingerprint(root+" ") || OriginCAFingerprint("") != "" {
		t.Fatal("fingerprint does not name the exact CA")
	}
}

// The snapshot route validator holds the same TLS equivalences as the agent: a CA exactly for custom_ca, a pin
// exactly for pinned, a verification mode for every https origin, and no mutual_tls mode.
func TestSnapshotRouteTLSEquivalences(t *testing.T) {
	root := testCertificatePEM(t, true)
	pin := "sha256:" + strings.Repeat("a", 64)
	badPin := "sha256:ABC"
	base := func() wireConfigRoute {
		return wireConfigRoute{ID: "route_1", Name: "app", Protocol: "http", MatchType: "catch_all", OriginScheme: "https", OriginAddress: "127.0.0.1:443",
			TLSVerification: "system", ConnectTimeoutMs: 10000, IdleTimeoutMs: 90000, MaxConcurrentStreams: 128, DesiredState: "active"}
	}
	for name, test := range map[string]struct {
		mutate func(*wireConfigRoute)
		ok     bool
	}{
		"system":                   {func(*wireConfigRoute) {}, true},
		"custom_ca with its CA":    {func(r *wireConfigRoute) { r.TLSVerification, r.TLSCA = "custom_ca", &root }, true},
		"custom_ca without a CA":   {func(r *wireConfigRoute) { r.TLSVerification = "custom_ca" }, false},
		"a CA on system":           {func(r *wireConfigRoute) { r.TLSCA = &root }, false},
		"pinned with its pin":      {func(r *wireConfigRoute) { r.TLSVerification, r.TLSPin = "pinned", &pin }, true},
		"pinned without a pin":     {func(r *wireConfigRoute) { r.TLSVerification = "pinned" }, false},
		"a malformed pin":          {func(r *wireConfigRoute) { r.TLSVerification, r.TLSPin = "pinned", &badPin }, false},
		"a pin on system":          {func(r *wireConfigRoute) { r.TLSPin = &pin }, false},
		"https without a mode":     {func(r *wireConfigRoute) { r.TLSVerification = "not_applicable" }, false},
		"mutual_tls is not a mode": {func(r *wireConfigRoute) { r.TLSVerification = "mutual_tls" }, false},
		"a server name on http": {func(r *wireConfigRoute) {
			name := "app.example.test"
			r.OriginScheme, r.TLSVerification, r.TLSServerName = "http", "not_applicable", &name
		}, false},
		"an mTLS reference on http": {func(r *wireConfigRoute) {
			reference := "paperboat://paperboat/mtls/1"
			r.OriginScheme, r.TLSVerification, r.MTLSCredentialReference = "http", "not_applicable", &reference
		}, false},
	} {
		route := base()
		test.mutate(&route)
		if err := validateWireConfigRoute(route); (err == nil) != test.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, test.ok)
		}
	}
}

// A stored config snapshot of a tunnel made before the route CA change carries "ca_reference":null; the shared validator
// (edge and server side) accepts it as the agent's parser does, and refuses a non-null reference.
func TestValidateConfigSnapshotPayloadAcceptsTheRetiredNullCAReference(t *testing.T) {
	stored := []byte(`{"access_mode":"public","desired_state":"active","expires_at":null,"generation":1,"kind":"tunnel_config_snapshot","name":"canary","routes":[{"ca_reference":null,"connect_timeout_ms":10000,"desired_state":"active","host_override":null,"id":"rte_tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03_default","idle_timeout_ms":90000,"match_hostname":"eager-purple-lantern-7514.tunnels.example.test","match_type":"managed_exact","max_concurrent_streams":1024,"mtls_credential_reference":null,"name":"default","origin_address":"127.0.0.1:18099","origin_scheme":"http","path_prefix":null,"preserve_host":true,"protocol":"http","tls_server_name":null,"tls_verification":"not_applicable"}],"schema":"paperboat.preview-tunnel/v1","stable_endpoint":"https://eager-purple-lantern-7514.tunnels.example.test","stable_endpoint_id":"endpoint_23bd453f-0558-4066-9d2e-d7e8a1b16bba","tunnel_id":"tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03"}`)
	if err := ValidateConfigSnapshotPayload(stored, "tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03", 1); err != nil {
		t.Fatalf("stored snapshot with ca_reference:null: %v", err)
	}
	withReference := []byte(strings.Replace(string(stored), `"ca_reference":null`, `"ca_reference":"cred_x"`, 1))
	if err := ValidateConfigSnapshotPayload(withReference, "tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03", 1); err == nil {
		t.Fatal("a non-null ca_reference was accepted")
	}
}
