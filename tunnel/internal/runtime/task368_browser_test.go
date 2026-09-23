package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgehttp"
)

// This fixture uses production admission pulls, acknowledgements, authenticated
// carriers and readiness observations. Only its private listener provisioning is
// task-owned; no preview route or ingress decision is manufactured here.
func TestTask368BrowserEdge(t *testing.T) {
	directory := os.Getenv("PAPERBOAT_TASK368_BROWSER_DIR")
	if directory == "" {
		t.Skip("explicit remote browser fixture only")
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("absolute task directory required")
	}
	raw, err := os.ReadFile(filepath.Join(directory, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var fixture struct {
		ControlURL string    `json:"control_url"`
		Credential string    `json:"edge_control_credential"`
		Domain     string    `json:"browser_domain"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	if json.Unmarshal(raw, &fixture) != nil || !fixture.ExpiresAt.After(time.Now()) {
		t.Fatal("invalid fixture")
	}
	ctx, cancel := context.WithDeadline(t.Context(), fixture.ExpiresAt.Add(-10*time.Second))
	defer cancel()
	cert, err := tls.LoadX509KeyPair(filepath.Join(directory, "server.crt"), filepath.Join(directory, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA")
	}
	source, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: fixture.ControlURL, Credential: fixture.Credential, Timeout: 15 * time.Second, TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	node, epoch := "task368-browser-edge", hex.EncodeToString(nonce[:])
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch, MaximumAdmissions: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer expected.Close()
	registry, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: fixture.Domain, ProcessEpoch: epoch, MaximumRoutes: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	worker, err := NewPreviewCarrierWorker(PreviewCarrierWorkerConfig{Source: source, Expected: expected, Registry: registry, NodeID: node, ProcessEpoch: epoch, Interval: time.Second, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewPreviewCarrierHandler(PreviewCarrierHandlerConfig{Source: source, Expected: expected, Registry: registry, NodeID: node, ProcessEpoch: epoch, WaitTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peerBinding := expected.PeerBinding
	carrierTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, VerifyConnection: func(state tls.ConnectionState) error { _, e := peerBinding(state); return e }}
	carrierConfig := datacarrier.DefaultConfig()
	carrierConfig.Authorize = expected
	carrier, err := datacarrier.NewHTTPService(ctx, datacarrier.ServiceConfig{Carrier: carrierConfig, TCP: &datacarrier.EndpointConfig{Address: "100.70.146.64:0", TLS: carrierTLS, PeerBinding: peerBinding}, QUIC: &datacarrier.EndpointConfig{Address: "100.70.146.64:0", TLS: carrierTLS, PeerBinding: peerBinding}})
	if err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			server, e := carrier.Accept(ctx)
			if e != nil {
				return
			}
			handlers.Add(1)
			go func() { defer handlers.Done(); defer server.Close(); _ = handler.Handle(ctx, server) }()
		}
	}()
	defer func() { cancel(); _ = carrier.Close(); <-acceptDone; handlers.Wait() }()
	if err = worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Shutdown(context.Background())
	transport, err := edgehttp.NewDataCarrierPreviewTransport(edgehttp.DataCarrierPreviewTransportConfig{Registry: registry, StreamOpenTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	browser := &control.BrowserAccessClient{HTTP: source, NodeID: node, ProcessEpoch: epoch}
	gateway, err := edgehttp.NewGatewayWithTransports(edgehttp.Config{BrowserAccess: &edgehttp.BrowserAccess{Authority: browser, LazyAuthority: browser, LoginOrigin: "https://login.pprbt.dev"}, PreviewBaseDomain: fixture.Domain, TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 16 << 10, MaxBodyBytes: 1 << 20, Routes: edgehttp.NewCompositeRouteMatcher(edgehttp.NewPreviewCarrierRouteMatcher(registry))}, "", transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "100.70.146.64:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	pem, err := os.ReadFile(filepath.Join(directory, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	port := func(addr net.Addr) int {
		_, text, e := net.SplitHostPort(addr.String())
		if e != nil {
			t.Fatal(e)
		}
		p, e := strconv.Atoi(text)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		snapshot := map[string]any{"node_id": node, "process_epoch": epoch, "http_address": listener.Addr().String(), "tcp_port": port(carrier.TCPAddr()), "quic_port": port(carrier.QUICAddr()), "spki": "sha256:" + hex.EncodeToString(digest[:]), "certificate": string(pem), "observed_at": time.Now().UTC(), "ready": true}
		encoded, e := json.Marshal(snapshot)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(directory, "browser-edge-ready.json"), encoded, 0600); e != nil {
			t.Fatal(e)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, e = os.Stat(filepath.Join(directory, "stop")); e == nil {
				return
			}
		}
	}
}
