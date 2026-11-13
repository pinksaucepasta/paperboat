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
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	edgeruntime "github.com/pinksaucepasta/paperboat/edge/internal/runtime"
)

type missSource struct {
	mu         sync.Mutex
	admissions []runtimewire.Admission
	pulls      int
	// pullDelay and observeDelay model a control plane that answers slowly (a replacement server's first requests).
	pullDelay, observeDelay time.Duration
}

func (s *missSource) RuntimeCarrierAdmissions(context.Context, string, string) ([]runtimewire.Admission, error) {
	s.mu.Lock()
	delay := s.pullDelay
	s.mu.Unlock()
	time.Sleep(delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls++
	return append([]runtimewire.Admission(nil), s.admissions...), nil
}

func (s *missSource) EndpointAdmissions(context.Context, string, string, string) (control.EndpointUpdate, error) {
	return control.EndpointUpdate{Complete: true, Cursor: "cursor"}, nil
}

func (s *missSource) ObserveRuntimeCarriers(ctx context.Context, _, _ string, _ []runtimewire.Observation) error {
	s.mu.Lock()
	delay := s.observeDelay
	s.mu.Unlock()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
	return nil
}

// Prod 2026-11-05 12:05:53: after an edge restart the server handed 21 hosts their admission for the
// new process up to one 5 s pull before the edge had it, and the edge refused every carrier at TLS
// ("data carrier TLS handshake failed"), so the hosts backed off. A carrier whose admission the
// server already issued must be accepted on its first dial, with no pull tick in between.
func TestCarrierAdmittedOnFirstDialWhenItsAdmissionIsNewerThanTheEdgePull(t *testing.T) {
	carrierAdmittedOnFirstDial(t, 0, 0)
}

// Gated deploys 2026-11-13 (.17, .20 server swaps): the replacement server answers its first requests
// slowly, and the handshake's wait covered the pull AND the observation report that follows it, so a
// machine that re-attached at the swap was refused unknown_machine after 3 s although the pull
// itself had answered in time. The wait ends with the pull.
func TestCarrierAdmittedOnFirstDialWhenTheControlPlaneObservationIsSlow(t *testing.T) {
	carrierAdmittedOnFirstDial(t, time.Second, 4*time.Second)
}

func carrierAdmittedOnFirstDial(t *testing.T, pullDelay, observeDelay time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const edgeNode, edgeProcess = "edge_miss", "edge_epoch_miss"
	tcpAddress, quicAddress := dynamicCarrierListenAddresses(t)
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(machinePublic)
	trust, err := control.NewProcessCarrierServerTrust(edgeNode, edgeProcess, "127.0.0.1", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	identity := datacarrier.Identity{AccountID: "account_miss", HostID: "host_miss", TunnelID: "tunnel_miss", ConnectorID: "connector_miss", SessionID: "session_miss", ProcessGeneration: 1, Generation: 1}
	admission := runtimewire.Admission{Schema: runtimewire.Schema, Binding: runtimewire.Binding{
		AccountID: identity.AccountID, HostID: identity.HostID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID,
		ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1, RouteID: "route_miss", RouteGeneration: 1,
		EdgeNodeID: edgeNode, EdgeProcessEpoch: edgeProcess, EdgeCarrierServerSPKISHA256: trust.SPKISHA256, EdgeCarrierServerCertificateChainPEM: trust.CertificateChainPEM,
		MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(machinePublic), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:]),
	}, AttachmentGeneration: 1, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EdgeEndpoints: []string{"h2://127.0.0.1:27443", "h3://127.0.0.1:27444"}, Hostname: "machine.runtime.example.test", RouteKind: runtimewire.RouteKind, RouteRevision: 1, ExpiresAt: time.Now().Add(time.Hour)}

	source := &missSource{}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: edgeNode, ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: edgeProcess})
	if err != nil {
		t.Fatal(err)
	}
	// The pull ticker is far longer than the test: only the handshake's own pull can admit the carrier.
	worker := &edgeruntime.RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpoints, Interval: 5 * time.Minute, Timeout: 10 * time.Second}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Shutdown(context.Background()) }()
	waitDynamicCondition(t, ctx, func() bool { source.mu.Lock(); defer source.mu.Unlock(); return source.pulls >= 1 })

	component, closeService, err := newCarrierComponentWithTelemetry(
		config.Deployment{CarrierTCPListenAddress: tcpAddress, CarrierQUICListenAddress: quicAddress, ControlInterval: time.Hour, NodeCapacity: 8},
		dynamicCarrierCertificate(t, "edge-carrier", nil, nil), nil, nil, nil, nil, nil, nil, nil, worker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if err := component.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = component.Shutdown(stop)
	}()

	// The server issues the admission now, after the edge's last pull.
	source.mu.Lock()
	source.admissions = []runtimewire.Admission{admission}
	source.pullDelay, source.observeDelay = pullDelay, observeDelay
	source.mu.Unlock()

	certificate, err := dynamicCarrierCertificateWithURI(t, "host-carrier", machinePublic, machinePrivate, identity, edgeProcess)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http2.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		InsecureSkipVerify: true, // This fixture's server certificate is self-signed.
		NextProtos:         []string{"h2"},
	}}
	defer transport.CloseIdleConnections()
	reader, writer := io.Pipe()
	defer writer.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+tcpAddress, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("first carrier dial refused although the server had issued its admission: %v", err)
	}
	defer response.Body.Close()
	clientConfig := datacarrier.DefaultConfig()
	clientConfig.Identity = identity
	clientConfig.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
	client, err := datacarrier.NewClient(ctx, dynamicHTTPLink{ReadCloser: response.Body, writer: writer}, clientConfig)
	if err != nil {
		t.Fatalf("create carrier client: %v", err)
	}
	defer client.Close()
	waitDynamicCondition(t, ctx, func() bool { _, ok := routes.Lookup("machine.runtime.example.test"); return ok })

	// Stopping this edge (an install handover) tells the host first, on its runtime route.
	drainer, ok := component.(interface {
		Drain(context.Context, time.Time)
	})
	if !ok {
		t.Fatal("carrier component does not drain")
	}
	deadline := time.Now().Add(2 * time.Second)
	go drainer.Drain(ctx, deadline)
	stream, open, err := client.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("host did not receive the edge goaway: %v", err)
	}
	defer stream.Close()
	if open.Kind != connectorprotocol.Goaway || open.RouteID != "route_miss" {
		t.Fatalf("edge goaway stream = %s on %s", open.Kind, open.RouteID)
	}
	notice, err := connectorprotocol.ReadGoawayNotice(stream)
	if err != nil || notice.Reason != connectorprotocol.GoawayEdgeDraining || notice.DrainDeadline != deadline.Unix() {
		t.Fatalf("edge goaway notice = %+v, %v", notice, err)
	}
}
