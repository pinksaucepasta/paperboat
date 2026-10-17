package tunnelenrollment

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connectorrotation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

type staleControlFixture struct {
	t           *testing.T
	now         time.Time
	hello       chan connectorprotocol.Hello
	bootstrap   chan carrierBootstrapRequest
	connections atomic.Int32
}

func (f *staleControlFixture) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/carrier-bootstrap") {
		var tuple carrierBootstrapRequest
		if err := json.NewDecoder(r.Body).Decode(&tuple); err != nil {
			f.t.Error(err)
			return
		}
		select {
		case f.bootstrap <- tuple:
		default:
			f.t.Error("unbounded bootstrap requests")
		}
		retryable := true
		payload := carrierBootstrapErrorPayload{Schema: carrierBootstrapErrorSchema, Kind: "error", Code: "connector_session_stale", Component: "control", Message: "The connector session changed before carrier bootstrap.", Outcome: "unchanged", Retryable: &retryable, RepairAction: "reconnect", RequestID: "request_stale_01", CorrelationID: "correlation_stale_01"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(carrierBootstrapErrorResponse{Error: payload})
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/control") {
		http.NotFound(w, r)
		return
	}
	f.serveControl(w, r)
}

func (f *staleControlFixture) serveControl(w http.ResponseWriter, r *http.Request) {
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{controlSubprotocol}})
	if err != nil {
		f.t.Error(err)
		return
	}
	defer connection.CloseNow()
	stream := websocket.NetConn(r.Context(), connection, websocket.MessageBinary)
	defer stream.Close()
	frame, err := connectorprotocol.ReadFrame(stream)
	if err != nil {
		return
	}
	var hello connectorprotocol.Hello
	if frame.Type != connectorprotocol.MessageHello || frame.DecodePayload(&hello) != nil {
		f.t.Error("control did not begin with Hello")
		return
	}
	count := f.connections.Add(1)
	f.hello <- hello
	session := fmt.Sprintf("session_live_%02d", count)
	welcome := connectorprotocol.Welcome{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, SessionID: session, Capabilities: hello.Capabilities, Lease: connectorprotocol.Lease{SessionID: session, ExpiresAt: f.now.Add(time.Minute), HeartbeatIntervalMS: 10000}, RequiresSnapshot: true, ServerTime: f.now}
	welcomeFrame, _ := connectorprotocol.NewFrame(connectorprotocol.MessageWelcome, "welcome_01", welcome)
	if err := connectorprotocol.WriteFrame(stream, welcomeFrame); err != nil {
		return
	}
	config := hoststate.TunnelConfigSnapshot{Schema: "paperboat.preview-tunnel/v1", Kind: "tunnel_config_snapshot", TunnelID: hello.TunnelID, Generation: 1, Name: "fixture", DesiredState: "active", AccessMode: "public", StableEndpointID: "endpoint_123e4567-e89b-42d3-a456-426614174000", StableEndpoint: "https://fixture.example.test", Routes: []hoststate.TunnelConfigRoute{{ID: "route_01", Name: "default", Protocol: "http", MatchType: "catch_all", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", PreserveHost: true, TLSVerification: "not_applicable", ConnectTimeoutMs: 10000, IdleTimeoutMs: 90000, MaxConcurrentStreams: 128, DesiredState: "active"}}}
	raw, _ := json.Marshal(config)
	snapshot, err := connectorprotocol.NewSnapshot(hello.TunnelID, 1, raw)
	if err != nil {
		f.t.Error(err)
		return
	}
	snapshot.AccountID, snapshot.ConnectorID, snapshot.SessionID, snapshot.ProcessGeneration = hello.AccountID, hello.ConnectorID, session, hello.ProcessGeneration
	snapshotFrame, _ := connectorprotocol.NewFrame(connectorprotocol.MessageSnapshot, "snapshot_01", snapshot)
	if err := connectorprotocol.WriteFrame(stream, snapshotFrame); err != nil {
		return
	}
	for {
		if _, err := connectorprotocol.ReadFrame(stream); err != nil {
			return
		}
	}
}

func TestTypedStaleBootstrapReconnectsControlBeforeRetryingTuple(t *testing.T) {
	now := time.Now().UTC()
	fixture := &staleControlFixture{t: t, now: now, hello: make(chan connectorprotocol.Hello, 8), bootstrap: make(chan carrierBootstrapRequest, 8)}
	server := httptest.NewTLSServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	source := newBootstrapSourceForTest(t, server, now, &bootstrapMachineAuth{})
	credentials, err := NewFileCredentialStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = source.BindCredentialStore(credentials); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	if err = source.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer source.Shutdown(context.Background())
	request, private := productionActivationRequest(t)
	signer := func(_ context.Context, payload []byte) ([]byte, error) { return ed25519.Sign(private, payload), nil }
	config, err := source.ResolveProductionAssembly(ctx, request, signer)
	if err != nil {
		t.Fatal(err)
	}
	// The assembly's existing authority boundary supplies a legitimately renewed,
	// signed process epoch. Recovery must request it instead of reusing the live stream.
	config.ControlSessionFactory = func(context.Context, *tunnelmanager.CoordinatedConfigApplier) (connectorrotation.ControlSessionConfig, error) {
		fresh := config.Control
		identity := ControlIdentity{AccountID: request.AccountID, ProcessGeneration: request.ProcessGeneration + 1, CredentialGeneration: request.CredentialGeneration}
		var err error
		fresh.Hello, err = source.newHello(ctx, request, identity, signer)
		return fresh, err
	}
	assembly, _, err := tunnelmanager.OpenProductionAssembly(config)
	if err != nil {
		t.Fatal(err)
	}
	defer assembly.Shutdown(context.Background())
	if err = assembly.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fixture.hello:
	case <-ctx.Done():
		t.Fatal("initial control Hello absent")
	}
	var initial carrierBootstrapRequest
	select {
	case initial = <-fixture.bootstrap:
	case <-ctx.Done():
		t.Fatal("initial carrier bootstrap absent")
	}
	var fresh connectorprotocol.Hello
	select {
	case fresh = <-fixture.hello:
	case <-time.After(3 * time.Second):
		t.Fatal("typed stale-session response retried without a new control Hello")
	}
	select {
	case retried := <-fixture.bootstrap:
		if retried.SessionID == initial.SessionID || retried.ProcessGeneration != fresh.ProcessGeneration || retried.ConfigGeneration != initial.ConfigGeneration || retried.ConfigContentHash != initial.ConfigContentHash {
			t.Fatal("bootstrap retry was not bound to fresh Welcome/Hello and the same desired snapshot")
		}
	case <-ctx.Done():
		t.Fatal("fresh control did not retry bootstrap")
	}
}
