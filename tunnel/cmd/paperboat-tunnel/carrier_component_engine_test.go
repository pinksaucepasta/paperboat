package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgehttp"
	edgeruntime "github.com/pinksaucepasta/paperboat/tunnel/internal/runtime"
)

// PLAN 3a step 4: an agent's runtime carrier that is already live becomes the replica of the
// agent's engine tunnel routes as soon as the durable admissions name its identity, and stops
// being one when they are withdrawn, without the carrier closing.
func TestRuntimeCarrierBecomesTheReplicaOfItsEngineTunnelRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	identity := datacarrier.Identity{
		AccountID: "account_engine", HostID: "host_engine", TunnelID: "runtime_engine",
		ConnectorID: "connector_engine", SessionID: "session_engine", ProcessGeneration: 3, Generation: 2,
	}
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	machinePublicEncoded := base64.RawURLEncoding.EncodeToString(machinePublic)
	machineThumbprint, err := connectorprotocol.IdentityThumbprint(machinePublic)
	if err != nil {
		t.Fatal(err)
	}
	const (
		edgeNodeID  = "edge_engine"
		edgeProcess = "_edge_engine_1"
		configHash  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	keyDigest := sha256.Sum256(machinePublic)
	now := time.Now().UTC()
	registry := func() *datacarrier.DurableAdmissionRegistry {
		r, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: edgeNodeID, ProcessEpoch: edgeProcess})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	durableExpected := registry()
	runtimeExpected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: edgeNodeID, ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeExpected.Close()
	runtimeAdmission := datacarrier.ExpectedAdmission{
		Schema: runtimewire.Schema, EdgeNodeID: edgeNodeID, EdgeProcessEpoch: edgeProcess, Identity: identity,
		InstallationGeneration: 1, ConfigGeneration: identity.Generation, ConfigContentHash: configHash,
		RouteID: "route_runtime", RouteKind: runtimewire.RouteKind, Hostname: "machine.runtime.example.test",
		RouteRevision: 1, AttachmentGeneration: 1, Endpoint: "https://machine.runtime.example.test", ExpiresAt: now.Add(5 * time.Minute),
		EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("0", 64), EdgeCarrierServerCertificateChainPEM: "test-certificate-chain",
		MachineIdentityPublicKey: machinePublicEncoded, MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(keyDigest[:]), Admitted: true,
	}
	if err := runtimeExpected.Replace([]datacarrier.ExpectedAdmission{runtimeAdmission}, now); err != nil {
		t.Fatalf("publish runtime admission: %v", err)
	}
	runtimeRoutes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeRoutes.Close()
	worker := &edgeruntime.RuntimeCarrierWorker{Expected: runtimeExpected, Routes: runtimeRoutes}
	routes, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer routes.Close()
	tcpAddress, quicAddress := dynamicCarrierListenAddresses(t)
	assembly, closeService, err := newCarrierComponentWithTelemetry(
		config.Deployment{CarrierTCPListenAddress: tcpAddress, CarrierQUICListenAddress: quicAddress, ControlInterval: time.Hour, NodeCapacity: 8},
		dynamicCarrierCertificate(t, "edge-carrier", nil, nil), durableExpected, nil,
		routes, nil, nil, nil, worker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if err := assembly.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = assembly.Shutdown(shutdownContext)
	}()

	carrierCertificate, err := dynamicCarrierCertificateWithURI(t, "agent-carrier", machinePublic, machinePrivate, identity, edgeProcess)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := datacarrier.DefaultConfig()
	clientConfig.Identity = identity
	clientConfig.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	transport := &http2.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{carrierCertificate},
		InsecureSkipVerify: true, // This fixture's server certificate is self-signed.
		NextProtos:         []string{"h2"},
	}}
	defer transport.CloseIdleConnections()
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+tcpAddress, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("dial HTTP2 carrier: %v", err)
	}
	client, err := datacarrier.NewClient(ctx, dynamicHTTPLink{ReadCloser: response.Body, writer: writer}, clientConfig)
	if err != nil {
		t.Fatalf("create carrier client: %v", err)
	}
	defer client.Close()

	// The runtime route is served first; no tunnel route names the carrier yet.
	waitDynamicCondition(t, ctx, func() bool { _, ok := runtimeRoutes.Lookup("machine.runtime.example.test"); return ok })
	if got := routes.Len(); got != 0 {
		t.Fatalf("tunnel route registry attached before any tunnel admission: %d", got)
	}
	// The ticker is an hour, so only Changed() can wake the carrier here.
	tunnelAdmission := datacarrier.DurableAdmission{
		Identity: identity, EdgeNodeID: edgeNodeID, EdgeProcessEpoch: edgeProcess,
		AssignmentID: "epa_route_tunnel", RouteID: "route_tunnel", AssignmentGeneration: 1,
		RouteGeneration: 1, ConfigContentHash: configHash,
		MachineIdentityPublicKey: machinePublicEncoded, MachineIdentityThumbprint: machineThumbprint,
		State: "staged", ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := durableExpected.Replace([]datacarrier.DurableAdmission{tunnelAdmission}, time.Now().UTC()); err != nil {
		t.Fatalf("publish tunnel admission: %v", err)
	}
	waitDynamicCondition(t, ctx, func() bool { return routes.Len() == 1 })
	if err := durableExpected.Replace(nil, time.Now().UTC()); err != nil {
		t.Fatalf("withdraw tunnel admission: %v", err)
	}
	waitDynamicCondition(t, ctx, func() bool { return routes.Len() == 0 })
	if _, ok := runtimeRoutes.Lookup("machine.runtime.example.test"); !ok {
		t.Fatal("runtime route lost when the tunnel routes were withdrawn")
	}
}

// A restarted machine reconnects before every edge has pulled its new runtime admission; when the
// engine tunnel admission arrived first, the edge accepted the carrier but never attached the runtime
// route on it, and reported the machine not ready for minutes (F8-03 on lanetrh-a, 22:01Z).
func TestRuntimeRouteAttachesWhenItsAdmissionArrivesAfterTheEngineCarrierConnected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	identity := datacarrier.Identity{
		AccountID: "account_engine", HostID: "host_engine", TunnelID: "runtime_engine",
		ConnectorID: "connector_engine", SessionID: "session_engine", ProcessGeneration: 3, Generation: 2,
	}
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	machinePublicEncoded := base64.RawURLEncoding.EncodeToString(machinePublic)
	machineThumbprint, err := connectorprotocol.IdentityThumbprint(machinePublic)
	if err != nil {
		t.Fatal(err)
	}
	const (
		edgeNodeID  = "edge_engine"
		edgeProcess = "_edge_engine_1"
		configHash  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	keyDigest := sha256.Sum256(machinePublic)
	now := time.Now().UTC()
	registry := func() *datacarrier.DurableAdmissionRegistry {
		r, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: edgeNodeID, ProcessEpoch: edgeProcess})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	durableExpected := registry()
	runtimeExpected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: edgeNodeID, ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeExpected.Close()
	runtimeAdmission := datacarrier.ExpectedAdmission{
		Schema: runtimewire.Schema, EdgeNodeID: edgeNodeID, EdgeProcessEpoch: edgeProcess, Identity: identity,
		InstallationGeneration: 1, ConfigGeneration: identity.Generation, ConfigContentHash: configHash,
		RouteID: "route_runtime", RouteKind: runtimewire.RouteKind, Hostname: "machine.runtime.example.test",
		RouteRevision: 1, AttachmentGeneration: 1, Endpoint: "https://machine.runtime.example.test", ExpiresAt: now.Add(5 * time.Minute),
		EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("0", 64), EdgeCarrierServerCertificateChainPEM: "test-certificate-chain",
		MachineIdentityPublicKey: machinePublicEncoded, MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(keyDigest[:]), Admitted: true,
	}
	// The edge learned the engine tunnel admission first (route pull) and not yet the new runtime
	// admission (runtime pull): the carrier is accepted on the durable admission alone.
	if err := durableExpected.Replace([]datacarrier.DurableAdmission{{
		Identity: identity, EdgeNodeID: edgeNodeID, EdgeProcessEpoch: edgeProcess,
		AssignmentID: "epa_route_tunnel", RouteID: "route_tunnel", AssignmentGeneration: 1,
		RouteGeneration: 1, ConfigContentHash: configHash,
		MachineIdentityPublicKey: machinePublicEncoded, MachineIdentityThumbprint: machineThumbprint,
		State: "active", ExpiresAt: now.Add(5 * time.Minute),
	}}, now); err != nil {
		t.Fatalf("publish tunnel admission: %v", err)
	}
	runtimeRoutes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeRoutes.Close()
	worker := &edgeruntime.RuntimeCarrierWorker{Expected: runtimeExpected, Routes: runtimeRoutes}
	routes, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer routes.Close()
	tcpAddress, quicAddress := dynamicCarrierListenAddresses(t)
	assembly, closeService, err := newCarrierComponentWithTelemetry(
		config.Deployment{CarrierTCPListenAddress: tcpAddress, CarrierQUICListenAddress: quicAddress, ControlInterval: time.Hour, NodeCapacity: 8},
		dynamicCarrierCertificate(t, "edge-carrier", nil, nil), durableExpected, nil,
		routes, nil, nil, nil, worker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if err := assembly.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = assembly.Shutdown(shutdownContext)
	}()

	carrierCertificate, err := dynamicCarrierCertificateWithURI(t, "agent-carrier", machinePublic, machinePrivate, identity, edgeProcess)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := datacarrier.DefaultConfig()
	clientConfig.Identity = identity
	clientConfig.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	transport := &http2.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{carrierCertificate},
		InsecureSkipVerify: true, // This fixture's server certificate is self-signed.
		NextProtos:         []string{"h2"},
	}}
	defer transport.CloseIdleConnections()
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+tcpAddress, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("dial HTTP2 carrier: %v", err)
	}
	client, err := datacarrier.NewClient(ctx, dynamicHTTPLink{ReadCloser: response.Body, writer: writer}, clientConfig)
	if err != nil {
		t.Fatalf("create carrier client: %v", err)
	}
	defer client.Close()

	waitDynamicCondition(t, ctx, func() bool { return routes.Len() == 1 })
	if _, ok := runtimeRoutes.Lookup("machine.runtime.example.test"); ok {
		t.Fatal("runtime route served before its admission")
	}
	if err := runtimeExpected.Replace([]datacarrier.ExpectedAdmission{runtimeAdmission}, time.Now().UTC()); err != nil {
		t.Fatalf("publish runtime admission: %v", err)
	}
	waitDynamicCondition(t, ctx, func() bool { _, ok := runtimeRoutes.Lookup("machine.runtime.example.test"); return ok })
}
