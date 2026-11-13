package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// readinessControl answers the edge the way the control plane does at an edge restart: the
// certificate distribution paths refuse a process that has not registered
// (server/internal/tunnelcert/distribution_identity_sql.go), and the runtime, private access and
// browser access paths refuse one the server has not recorded as ready
// (server/internal/previewattachment/identity.go verifyPreviewEdgeNodeSQL).
type readinessControl struct {
	t         *testing.T
	revoked   string
	jwks      string
	mu        sync.Mutex
	epoch     string // registered process epoch
	ready     bool
	refusals  []string
	readyPull map[string]bool // ready-gated paths answered after ready
}

func readyGated(path string) bool {
	return path == runtimewire.EdgePullPath || path == runtimewire.EdgeObservePath || path == runtimewire.EdgeEndpointPullPath ||
		path == control.PrivateAccessCarrierAdmissionsPath || path == control.PrivateAccessGrantAuthorizePath ||
		strings.HasPrefix(path, "/v1/edge/browser-access/")
}

func (c *readinessControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		ProcessEpoch string `json:"process_epoch"`
		Ready        bool   `json:"ready"`
	}
	_ = json.Unmarshal(body, &in)
	c.mu.Lock()
	defer c.mu.Unlock()
	refuse := func() { c.refusals = append(c.refusals, r.URL.Path); w.WriteHeader(http.StatusUnauthorized) }
	epoch := r.Header.Get("X-Paperboat-Edge-Process-Epoch")
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/edge/certificates/distributions/") && (c.epoch == "" || epoch != c.epoch):
		refuse()
		return
	case readyGated(r.URL.Path) && !c.ready:
		refuse()
		return
	case readyGated(r.URL.Path):
		c.readyPull[r.URL.Path] = true
	}
	answer := func(v string) { w.Header().Set("Content-Type", "application/json"); _, _ = io.WriteString(w, v) }
	switch r.URL.Path {
	case "/v1/nodes/register":
		c.epoch, c.ready = in.ProcessEpoch, false
		w.WriteHeader(http.StatusNoContent)
	case "/v1/nodes/heartbeat":
		c.ready = c.ready || in.Ready && in.ProcessEpoch == c.epoch
		answer(`{}`)
	case "/v1/trust/revocations":
		answer(c.revoked)
	case "/v1/trust/jwks":
		answer(c.jwks)
	case "/v1/edge/routes/desired-state":
		answer(`{"complete":true,"version":"v1","routes":[]}`)
	case "/v1/edge/routes/observations":
		w.WriteHeader(http.StatusNoContent)
	case "/v1/edge/ingress/desired-state":
		answer(fmt.Sprintf(`{"version":"v1","issued_at":%q,"expires_at":[],"complete":true,"decisions":[]}`, time.Now().UTC().Format(time.RFC3339Nano)))
	case control.CertificateDistributionPullPath:
		answer(`{"version":1,"complete":true,"messages":[]}`)
	case runtimewire.EdgePullPath:
		answer(fmt.Sprintf(`{"schema":%q,"complete":true,"admissions":[]}`, runtimewire.Schema))
	case runtimewire.EdgeEndpointPullPath:
		answer(fmt.Sprintf(`{"schema":%q,"complete":true,"carriers":[],"cursor":"c1"}`, runtimewire.Schema))
	case control.PrivateAccessCarrierAdmissionsPath:
		answer(`{"schema":"paperboat.preview-tunnel/v1","kind":"private_access_carrier_snapshot","complete":true,"admissions":[]}`)
	default:
		c.t.Errorf("unexpected control request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func freePort(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.LocalAddr().String()
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// An edge restart through the real service assembly: the control plane refuses nothing, and the
// pulls it only answers for a ready node run once the edge is ready.
func TestEdgeStartupSendsNoControlRequestTheServerRefuses(t *testing.T) {
	const nodeID = "edge-ready"
	revoked := `{"jtis":[],"environments":[],"connector_generations":[],"key_ids":[]}`
	fake := &readinessControl{t: t, revoked: revoked, readyPull: map[string]bool{}}
	server := httptest.NewTLSServer(fake)
	defer server.Close()

	dir := t.TempDir()
	write := func(name, value string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	jwks := fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"connector","x":"%s"}]}`, base64.RawURLEncoding.EncodeToString(public))
	fake.mu.Lock()
	fake.jwks = jwks
	fake.mu.Unlock()
	carrierTrust, err := control.NewProcessCarrierServerTrust(nodeID, "installation", "edge.example.test", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	carrierKey, err := x509.MarshalPKCS8PrivateKey(carrierTrust.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	deployment := map[string]any{
		"infrastructure_tls_cert_file": write("tls.crt", carrierTrust.CertificateChainPEM, 0o600),
		"infrastructure_tls_key_file":  write("tls.key", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: carrierKey})), 0o600),
		"control_url":                  server.URL,
		"control_ca_file":              write("control-ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})), 0o644),
		"control_credential_file":      write("control.credential", strings.Repeat("c", 48), 0o600),
		"jwks_file":                    write("jwks.json", jwks, 0o644),
		"revocations_file":             write("revocations.json", revoked, 0o644),
		"usage_signing_key_file":       write("usage.key", fmt.Sprintf(`{"key_id":"usage","edge_node_id":%q,"private_key":"%s"}`, nodeID, base64.RawURLEncoding.EncodeToString(private)), 0o600),
		"connector_advertise_host":     "edge.example.test",
		"carrier_tcp_listen_address":   freePort(t, "tcp"),
		"carrier_quic_listen_address":  freePort(t, "udp"),
		"public_https_listen_address":  freePort(t, "tcp"),
		"private_https_listen_address": freePort(t, "tcp"),
		"public_http_listen_address":   freePort(t, "tcp"),
		"preview_base_domain":          "preview.example.test",
		"tunnel_base_domain":           "tunnels.example.test",
		"runtime_base_domain":          "runtime.example.test",
		"trusted_proxy_cidrs":          []string{"127.0.0.1/32"},
		"node_capacity":                8,
		"control_interval":             time.Second,
		"control_timeout":              5 * time.Second,
	}
	encoded, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadDeployment(write("deployment.json", string(encoded), 0o644))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{"--node-id", nodeID, "--health-address", freePort(t, "tcp"), "--state-path", filepath.Join(dir, "state.json"), "--deployment-config", filepath.Join(dir, "deployment.json"), "--shutdown-timeout", "10s"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := buildServiceWithReporter(cfg, loaded, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelStop()
		// shortcut: shutdown reports the HTTP/3 listener's http.ErrServerClosed (http3server.go);
		// this test covers startup only, so it logs shutdown errors instead of failing on them.
		if err := service.Shutdown(stop); err != nil {
			t.Logf("shutdown: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fake.mu.Lock()
		done := fake.readyPull[runtimewire.EdgePullPath] && fake.readyPull[runtimewire.EdgeEndpointPullPath] && fake.readyPull[control.PrivateAccessCarrierAdmissionsPath]
		refusals, pulls := append([]string(nil), fake.refusals...), fmt.Sprint(fake.readyPull)
		fake.mu.Unlock()
		if len(refusals) != 0 {
			t.Fatalf("control refused requests the edge sent before it was ready: %v", refusals)
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ready-gated pulls did not run after readiness: %s", pulls)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
