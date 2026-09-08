package tunnelenrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

func TestTask24ProductionBootstrapInstallsAuthenticatedIngressLookup(t *testing.T) {
	now := time.Now().UTC()
	request, private := productionActivationRequest(t)
	auth := &bootstrapMachineAuth{}
	var calls atomic.Int32
	var descriptor carrierBootstrapDescriptor
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != rpath(request) || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Paperboat-Machine-Identity") != strings.Repeat("i", 48) || r.Header.Get("X-Paperboat-Machine-Proof") == "" || r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("lookup did not use exact machine-proof bootstrap request")
		}
		w.Header().Set("Content-Type", "application/json")
		current := descriptor
		if calls.Load() >= 3 {
			current.IngressDecisions = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": current})
	}))
	defer server.Close()
	chainPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	spki := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	decision := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_01", AccountID: request.AccountID, TunnelID: request.TunnelID, Lifecycle: connectorprotocol.TunnelDurable, ResourceGeneration: 1, RouteID: "route_01", RouteGeneration: 1, TargetID: "target_01", TargetGeneration: 1, HostID: request.HostID, InstallationGeneration: 1, Audience: "public", ConnectionMethod: "edge", Protocol: "http", Hostname: "app.customer.test", PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:8080", TLSVerification: "not_applicable", PublicationID: "publication_01", PublicationGeneration: 1}, DecisionID: "decision_01", PolicyGeneration: 1, EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", ConnectorID: request.ConnectorID, SessionID: "session_live_01", ProcessGeneration: request.ProcessGeneration, ConfigGeneration: 7, AssignmentGeneration: 1, Action: "view", IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(9 * time.Second)}
	descriptor = carrierBootstrapDescriptor{Schema: carrierBootstrapSchema, Kind: "carrier_bootstrap_descriptor", AccountID: request.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID, HostID: request.HostID, StableEndpointID: request.StableEndpointID, SessionID: decision.SessionID, ProcessGeneration: request.ProcessGeneration, CredentialGeneration: request.CredentialGeneration, ConfigGeneration: decision.ConfigGeneration, ConfigContentHash: "sha256:" + strings.Repeat("a", 64), Carriers: []carrierBootstrapNode{{EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", FailureDomain: "zone_a", Endpoints: []string{"h2://127.0.0.1:4443", "h3://127.0.0.1:4444"}, ServerSPKISHA256: "sha256:" + hex.EncodeToString(spki[:]), ServerCertificateChainPEM: string(chainPEM)}}, IngressDecisions: []connectorprotocol.IngressDecision{decision}, IssuedAt: now, ExpiresAt: now.Add(15 * time.Second)}
	source := newBootstrapSourceForTest(t, server, now, auth)
	store, err := NewFileCredentialStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := source.BindCredentialStore(store); err != nil {
		t.Fatal(err)
	}
	if err := source.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer source.Shutdown(context.Background())
	injected := &tunnelmanager.OriginStreamForwarder{IngressAuthority: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return connectorprotocol.IngressDecision{}, errors.New("injected authority used")
	}}
	source.originStreams = injected
	config, err := source.ResolveProductionAssembly(context.Background(), request, func(_ context.Context, payload []byte) ([]byte, error) { return ed25519.Sign(private, payload), nil })
	if err != nil {
		t.Fatal(err)
	}
	welcome := connectorprotocol.Welcome{SessionID: decision.SessionID}
	apply := tunnelmanager.ApplyRequest{Snapshot: hoststate.ConfigSnapshot{Generation: decision.ConfigGeneration, ContentHash: descriptor.ConfigContentHash}}
	if _, err := config.CarrierDescriptorSource(context.Background(), welcome, apply); err != nil {
		t.Fatal(err)
	}
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: request.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID, SessionID: decision.SessionID, ProcessGeneration: request.ProcessGeneration, Generation: decision.ConfigGeneration, RouteID: decision.Binding.RouteID, RequestID: "request_01", Kind: "http"}
	got, err := config.OriginStreams.IngressAuthority(context.Background(), open, decision)
	if err != nil || got != decision {
		t.Fatalf("installed lookup=%+v err=%v", got, err)
	}
	auth.mu.Lock()
	var proofBody carrierBootstrapRequest
	proofOK := auth.operation != "" && auth.method == http.MethodPost && auth.path == rpath(request) && json.Unmarshal(auth.body, &proofBody) == nil && proofBody.SessionID == decision.SessionID && proofBody.ConfigGeneration == decision.ConfigGeneration
	auth.mu.Unlock()
	if !proofOK {
		t.Fatal("machine proof did not bind exact lookup tuple/body")
	}
	time.Sleep(connectorprotocol.IngressRefreshInterval + 50*time.Millisecond)
	if _, err := config.OriginStreams.IngressAuthority(context.Background(), open, decision); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("revoked refresh error=%v", err)
	}
}
