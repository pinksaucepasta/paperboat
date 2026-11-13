package edgehttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func TestPrivateDurableGrantReachesTLSHTTPIngress(t *testing.T) {
	testPrivateDurableGrantReachesTLSHTTPIngress(t, false, nil)
}

// A private connection outlives the 10 s ingress decision it was admitted with: a request sent on it
// after that decision expired reaches the origin on a decision the server revalidated, which the
// connection keeps for its next requests; a revalidation the server refuses (revoked) is refused.
func TestPrivateDurableRequestAfterAdmissionDecisionExpiry(t *testing.T) {
	testPrivateDurableGrantReachesTLSHTTPIngress(t, true, nil)
	testPrivateDurableGrantReachesTLSHTTPIngress(t, true, connectorprotocol.ErrIngressDenied)
}

func testPrivateDurableGrantReachesTLSHTTPIngress(t *testing.T, afterExpiry bool, revalidateErr error) {
	now := time.Now().UTC()
	issued := now
	if afterExpiry {
		issued = now.Add(-connectorprotocol.IngressAuthorityLifetime + time.Second) // expires 1 s from now
	}
	refused := revalidateErr != nil
	identity := testEdgePreviewIdentity(2, 3)
	id := func(noun string) string { return noun + "_11111111-1111-4111-8111-111111111111" }
	identity.AccountID, identity.TunnelID, identity.ConnectorID, identity.HostID, identity.SessionID = id("user"), id("machine"), id("machine"), id("machine"), id("session")
	server, client := testEdgePreviewCarrierPair(t, identity)
	accessorIdentity := identity
	accessorIdentity.HostID = "machine_22222222-2222-4222-8222-222222222222"
	accessorServer, accessorClient := testEdgePreviewCarrierPair(t, accessorIdentity)
	request := edgePrivateAccessRequest(now)
	request.AccountID, request.MachineID, request.CarrierSessionID = identity.AccountID, accessorIdentity.HostID, identity.SessionID
	request.ResourceID, request.RouteID, request.OperationID = id("preview"), id("route"), id("operation")
	request.SessionID, request.IdempotencyKey, request.RequestID, request.CorrelationID = id("session"), id("operation"), id("request"), id("correlation")
	request.ResourceKind, request.ResourceID, request.ConnectorID, request.Audience = "tunnel", identity.TunnelID, identity.ConnectorID, "paperboat-tunnel-http"
	request.ProcessGeneration, request.ConfigGeneration = identity.ProcessGeneration, identity.Generation
	request.SessionGeneration, request.AssignmentGeneration = 1, 1
	request.EdgeProcessEpoch = "epoch_private_preview_12345678"
	rule := replicaRule()
	rule.Generation, rule.Revision = 1, request.RouteGeneration
	rule.Target, rule.ObservedState = request.RouteID, "ready"
	rule.ID, rule.RouteID, rule.AccountID, rule.TunnelID, rule.HostID = request.RouteID, request.RouteID, identity.AccountID, identity.TunnelID, identity.HostID
	rule.ConnectorID, rule.ConnectorSessionID = identity.ConnectorID, identity.SessionID
	rule.ConnectorProcessGeneration, rule.ConfigGeneration, rule.SessionGeneration = identity.ProcessGeneration, identity.Generation, request.SessionGeneration
	rule.RouteGeneration, rule.AssignmentGeneration = request.RouteGeneration, request.AssignmentGeneration
	rule.AccessMode, rule.ResourceKind, rule.Hostname, rule.PathPrefix, rule.MatchType = "private", "tunnel", request.Host, "/", route.MatchExact
	rule.Node, rule.EdgeProcessEpoch = request.EdgeNodeID, request.EdgeProcessEpoch
	rule.ViewerPolicyGeneration = 1
	published, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if err = published.AttachReplica(server, "key", "thumb", replicaState(rule, identity.Generation, rule.AssignmentID, "test", time.Millisecond, 4)); err != nil {
		t.Fatal(err)
	}
	transport, err := NewDataCarrierRouteTransport(DataCarrierRouteTransportConfig{Registry: published})
	if err != nil {
		t.Fatal(err)
	}
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	if err = routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}

	evidence := connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed-grant", Request: request}
	decision := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "environment_test", AccountID: request.AccountID, TunnelID: request.ResourceID, Lifecycle: "durable", ResourceGeneration: 1, RouteID: request.RouteID, RouteGeneration: request.RouteGeneration, TargetID: request.RouteID, TargetGeneration: request.RouteGeneration, HostID: identity.HostID, InstallationGeneration: 1, Audience: "private", ConnectionMethod: "edge", Protocol: "http", Hostname: request.Host, PathPrefix: "/", OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", PublicationID: request.RouteID, PublicationGeneration: request.RouteGeneration}, DecisionID: "decision_test", PolicyGeneration: 1, EdgeNodeID: request.EdgeNodeID, EdgeProcessEpoch: request.EdgeProcessEpoch, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, AssignmentGeneration: request.AssignmentGeneration, PrincipalID: request.MachineID, GrantID: request.Nonce, GrantGeneration: request.InstallationGeneration, Action: "view", IssuedAt: issued, ExpiresAt: issued.Add(connectorprotocol.IngressAuthorityLifetime), NativeAuthorization: &evidence}
	if err = decision.Validate(now); err != nil {
		t.Fatal(err)
	}
	originDone := make(chan error, 1)
	go func() {
		stream, open, err := client.AcceptStream(context.Background())
		if err != nil {
			originDone <- err
			return
		}
		defer stream.Close()
		if open.Kind != "http" || open.RouteID != request.RouteID || open.SessionID != identity.SessionID {
			originDone <- fmt.Errorf("incorrect private origin stream binding")
			return
		}
		received, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC())
		if err != nil || received.NativeAuthorization == nil || *received.NativeAuthorization != evidence {
			originDone <- fmt.Errorf("missing exact native ingress authority: %v", err)
			return
		}
		originDone <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "private origin reached") }), 4096, time.Second)
	}()
	registry, _ := NewPrivateAccessConnectionRegistry(8)
	var revalidations atomic.Int32
	registry.Revalidate = func(_ context.Context, current connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		revalidations.Add(1)
		current.IssuedAt = time.Now().UTC()
		current.ExpiresAt = current.IssuedAt.Add(connectorprotocol.IngressAuthorityLifetime)
		return current, revalidateErr
	}
	policy, err := New(Config{SelfHosted: true, MaxHeaderBytes: 4096, MaxBodyBytes: 1024, Routes: routes, PrivateAccessConnections: registry}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := transport.RoundTrip(r)
		if err != nil {
			if !refused {
				t.Errorf("private origin transport: %v", err)
			}
			http.Error(w, "origin failed", 502)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(policy)
	defer tlsServer.Close()
	bridge, err := NewPrivateAccessStreamBridge(PrivateAccessStreamBridgeConfig{Authorizer: privateAccessGrantAuthorizerFunc(func(_ context.Context, _ string, got connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
		if got != request {
			t.Error("grant binding changed")
		}
		return control.PrivateAccessGrantDecision{Allowed: true, ExpiresAt: request.ExpiresAt, Ingress: &decision}, nil
	}), Target: PrivateAccessHTTPTarget{Address: tlsServer.Listener.Addr().String(), Connections: registry}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bridge.Serve(ctx, accessorServer)
	stream, err := accessorClient.OpenStream(ctx, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: request.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: request.CarrierSessionID, ProcessGeneration: request.ProcessGeneration, Generation: request.ConfigGeneration, RouteID: request.RouteID, RequestID: request.RequestID, Kind: connectorprotocol.PrivateAccessHTTP})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err = connectorprotocol.WritePrivateAccessOpen(stream, connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed-grant", Request: request}); err != nil {
		t.Fatal(err)
	}
	result, err := connectorprotocol.ReadPrivateAccessResult(stream, now)
	if err != nil || result.Status != 200 {
		t.Fatalf("open=%+v err=%v", result, err)
	}
	config := tlsServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	config.ServerName = "example.com"
	secured := tls.Client(privateTLSStreamConn{stream}, config)
	defer secured.Close()
	secured.SetDeadline(time.Now().Add(5 * time.Second))
	if err = secured.Handshake(); err != nil {
		t.Fatal(err)
	}
	if afterExpiry {
		time.Sleep(time.Until(decision.ExpiresAt) + 200*time.Millisecond)
		secured.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err = io.WriteString(secured, "GET / HTTP/1.1\r\nHost: "+request.Host+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(secured), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if refused {
		if response.StatusCode == 200 || revalidations.Load() != 1 {
			t.Fatalf("revoked revalidation: status=%d revalidations=%d", response.StatusCode, revalidations.Load())
		}
		return
	}
	if err != nil || response.StatusCode != 200 || string(body) != "private origin reached" {
		t.Fatalf("HTTP status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	if afterExpiry {
		// The refreshed decision is kept: the connection's next request does not ask the server again.
		registry.mu.Lock()
		var address string
		for key := range registry.entries {
			address = key
		}
		registry.mu.Unlock()
		if _, ok := registry.ingress(context.Background(), address, route.RouteMatch{Rule: rule, Host: request.Host}); !ok || revalidations.Load() != 1 {
			t.Fatalf("refreshed decision not kept: ok=%v revalidations=%d", ok, revalidations.Load())
		}
	} else if revalidations.Load() != 0 {
		t.Fatalf("a current admission decision was revalidated %d times", revalidations.Load())
	}
	select {
	case err := <-originDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("origin stream did not close")
	}
}
