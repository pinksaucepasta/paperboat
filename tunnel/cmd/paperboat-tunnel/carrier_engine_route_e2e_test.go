package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
	edgeruntime "github.com/pinksaucepasta/paperboat/tunnel/internal/runtime"
)

const sharedTestdata = "../../../shared/runtimeattachment/testdata"

// PLAN 3a step 4, end to end on the edge: the engine route row the server really sends (golden, from
// TestTunnelsMoveOntoTheEngineOnlyWhenAgentAndEdgesServeThemOnPostgres) goes through the edge's real
// route decoder and the route worker's real conversions into the real carrier component, with the
// route registry built as production builds it (with an ingress authority, so readiness is a
// connector_ready probe). The agent's runtime carrier becomes the route's replica and the probe is
// answered, so the route can be observed ready. Round 21 and round 23 each failed one of these steps
// on prod edges while every unit test passed. The probe as the agent receives it is written for the
// agent's own contract test (cli runtimeattachment).
func TestEngineRouteGoesReadyThroughTheRealEdgeCarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	golden, err := os.ReadFile(filepath.Join(sharedTestdata, "engine_route_snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	control := goldenRouteServer(t, golden)
	var probe struct {
		Routes []struct {
			Node  string `json:"edge_node_id"`
			Epoch string `json:"edge_process_epoch"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(golden, &probe); err != nil || len(probe.Routes) != 1 {
		t.Fatalf("golden: %v", err)
	}
	node, epoch := probe.Routes[0].Node, probe.Routes[0].Epoch
	snapshot, err := control.DesiredRouteSnapshot(ctx, node, epoch)
	if err != nil || len(snapshot.Routes) != 1 {
		t.Fatalf("route decoder: %+v %v", snapshot, err)
	}
	assignment := snapshot.Routes[0]
	admissions, err := edgeruntime.CanonicalDurableAdmissions(snapshot.Routes)
	if err != nil {
		t.Fatal(err)
	}
	rules := edgeruntime.CanonicalRouteRules(snapshot.Routes)
	if len(rules) != 1 {
		t.Fatalf("rules = %d", len(rules))
	}
	// The agent: the machine key from the shared seed, the identity of its runtime carrier.
	seed, err := os.ReadFile(filepath.Join(sharedTestdata, "engine_machine_seed.hex"))
	if err != nil {
		t.Fatal(err)
	}
	seedBytes, err := hex.DecodeString(strings.TrimSpace(string(seed)))
	if err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(seedBytes)
	public := private.Public().(ed25519.PublicKey)
	if base64.RawURLEncoding.EncodeToString(public) != assignment.MachineIdentityPublicKey {
		t.Fatal("golden machine key is not the shared seed's key: rewrite the golden")
	}
	identity := admissions[0].Identity
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	previewExpected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer previewExpected.Close()
	runtimeExpected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeExpected.Close()
	keyDigest := sha256.Sum256(public)
	now := time.Now().UTC()
	if err := runtimeExpected.Replace([]datacarrier.ExpectedAdmission{{
		Schema: runtimewire.Schema, EdgeNodeID: node, EdgeProcessEpoch: epoch, Identity: identity,
		InstallationGeneration: 1, ConfigGeneration: identity.Generation, ConfigContentHash: assignment.ConfigContentHash,
		RouteID: "route_runtime_e2e", RouteKind: runtimewire.RouteKind, Hostname: "machine.runtime.example.test",
		RouteRevision: 1, AttachmentGeneration: 1, Endpoint: "https://machine.runtime.example.test", ExpiresAt: now.Add(5 * time.Minute),
		EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("0", 64), EdgeCarrierServerCertificateChainPEM: "test-certificate-chain",
		MachineIdentityPublicKey: assignment.MachineIdentityPublicKey, MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(keyDigest[:]), Admitted: true,
	}}, now); err != nil {
		t.Fatalf("runtime admission: %v", err)
	}
	runtimeRoutes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeRoutes.Close()
	// As in production (main.go): the durable route registry has an ingress authority.
	routes, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 8,
		IngressAuthority: func(context.Context, route.RouteRule) (connectorprotocol.IngressDecision, error) {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer routes.Close()
	tcpAddress, quicAddress := dynamicCarrierListenAddresses(t)
	assembly, closeService, err := newCarrierComponentWithTelemetry(
		config.Deployment{CarrierTCPListenAddress: tcpAddress, CarrierQUICListenAddress: quicAddress, ControlInterval: time.Hour, NodeCapacity: 8},
		dynamicCarrierCertificate(t, "edge-carrier", nil, nil), previewExpected, durable, nil,
		func(context.Context, *datacarrier.Server) error { return nil }, routes, nil, nil, nil,
		&edgeruntime.RuntimeCarrierWorker{Expected: runtimeExpected, Routes: runtimeRoutes},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if err := assembly.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdown, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = assembly.Shutdown(shutdown)
	}()
	agent := dialEngineAgent(t, ctx, tcpAddress, public, private, identity, epoch)
	waitDynamicCondition(t, ctx, func() bool { _, ok := runtimeRoutes.Lookup("machine.runtime.example.test"); return ok })
	if err := durable.Replace(admissions, time.Now().UTC()); err != nil {
		t.Fatalf("the edge refused the server's engine route: %v", err)
	}
	waitDynamicCondition(t, ctx, func() bool { return routes.Len() == 1 })
	// The readiness probe reaches the agent's carrier; the agent acknowledges it as the real agent does.
	received := make(chan connectorprotocol.StreamOpen, 1)
	go func() {
		stream, open, err := agent.AcceptStream(ctx)
		if err != nil {
			return
		}
		received <- connectorprotocol.StreamOpen(open)
		if open.Kind == "connector_ready" {
			_, _ = stream.Write([]byte{1})
		}
		_ = stream.Close()
	}()
	if err := routes.ProbeRoutes(ctx, rules); err != nil {
		t.Fatalf("readiness probe of the engine route failed: %v", err)
	}
	open := <-received
	if open.Kind != "connector_ready" || open.RouteID != assignment.RouteID {
		t.Fatalf("probe = %+v", open)
	}
	if dir := os.Getenv("PAPERBOAT_UPDATE_GOLDEN"); dir != "" {
		encoded, _ := json.MarshalIndent(map[string]string{"kind": open.Kind, "route_id": open.RouteID}, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "engine_probe_open.json"), append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func goldenRouteServer(t *testing.T, golden []byte) *control.HTTPClient {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(golden) }))
	t.Cleanup(server.Close)
	client, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// dialEngineAgent connects the agent's runtime carrier with the machine key's certificate.
func dialEngineAgent(t *testing.T, ctx context.Context, address string, public ed25519.PublicKey, private ed25519.PrivateKey, identity datacarrier.Identity, epoch string) *datacarrier.Client {
	t.Helper()
	certificate, err := dynamicCarrierCertificateWithURI(t, "agent-carrier", public, private, identity, epoch)
	if err != nil {
		t.Fatal(err)
	}
	config := datacarrier.DefaultConfig()
	config.Identity = identity
	config.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	transport := &http2.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		InsecureSkipVerify: true, // This fixture's edge certificate is self-signed.
		NextProtos:         []string{"h2"}}}
	t.Cleanup(transport.CloseIdleConnections)
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+address, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("dial the edge carrier: %v", err)
	}
	client, err := datacarrier.NewClient(ctx, dynamicHTTPLink{ReadCloser: response.Body, writer: writer}, config)
	if err != nil {
		t.Fatalf("carrier client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
