package edgehttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	yamux "github.com/libp2p/go-yamux/v5"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

type privateAccessGrantAuthorizerFunc func(context.Context, string, connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error)

func (f privateAccessGrantAuthorizerFunc) AuthorizePrivateAccessGrant(ctx context.Context, grant string, request connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
	return f(ctx, grant, request)
}

type privateTCPRouteSnapshot struct{ rules []route.RouteRule }

func (s privateTCPRouteSnapshot) CanonicalSnapshot() (uint64, []route.RouteRule, bool) {
	return 1, append([]route.RouteRule(nil), s.rules...), true
}

func TestPrivateTCPFullAuthenticatedEdgePath(t *testing.T) {
	now := time.Now().UTC()
	durableIdentity := datacarrier.Identity{AccountID: "account_1", HostID: "owner_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 2, Generation: 3}
	accessorIdentity := durableIdentity
	accessorIdentity.HostID = "accessor_1"
	request := edgePrivateAccessRequest(now)
	request.AccountID, request.MachineID = accessorIdentity.AccountID, accessorIdentity.HostID
	request.ResourceKind, request.ResourceID = "tunnel", durableIdentity.TunnelID
	request.Audience, request.Protocol = "paperboat-tunnel-tcp", "tcp"
	request.Method, request.Host, request.Path = "", "", ""
	request.ConnectorID, request.CarrierSessionID = durableIdentity.ConnectorID, durableIdentity.SessionID
	request.ProcessGeneration, request.ConfigGeneration = durableIdentity.ProcessGeneration, durableIdentity.Generation
	rule := route.RouteRule{ID: request.RouteID, RouteID: request.RouteID, RouteGeneration: request.RouteGeneration, AssignmentID: "assignment_private_tcp", AssignmentGeneration: request.AssignmentGeneration, ResourceKind: "tunnel", SessionGeneration: request.SessionGeneration, AccountID: request.AccountID, HostID: durableIdentity.HostID, TunnelID: request.ResourceID, ConnectorID: request.ConnectorID, ConnectorSessionID: request.CarrierSessionID, ConnectorProcessGeneration: request.ProcessGeneration, ConfigGeneration: request.ConfigGeneration, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Node: request.EdgeNodeID, EdgeProcessEpoch: request.EdgeProcessEpoch, Kind: route.TunnelPrivateTCP, Protocol: "private_tcp", AccessMode: "private"}

	t.Run("allow duplex", func(t *testing.T) {
		accessorServer, accessorClient := testEdgePreviewCarrierPair(t, accessorIdentity)
		durableServer, durableClient := testEdgePreviewCarrierPair(t, durableIdentity)
		registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 4})
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		state := replicaState(rule, durableIdentity.Generation, rule.AssignmentID, "zone_1", time.Millisecond, 4)
		if err := registry.AttachReplica(durableServer, "owner-key", "owner-thumb", state); err != nil {
			t.Fatal(err)
		}
		routeTarget := PrivateAccessRouteTarget{HTTP: PrivateAccessTargetFunc(func(context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
			t.Fatal("TCP reached Caddy target")
			return nil, nil
		}), Routes: privateTCPRouteSnapshot{rules: []route.RouteRule{rule}}, Carriers: registry}
		bridge, err := NewPrivateAccessStreamBridge(PrivateAccessStreamBridgeConfig{Authorizer: privateAccessGrantAuthorizerFunc(func(context.Context, string, connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
			return control.PrivateAccessGrantDecision{Allowed: true, ExpiresAt: now.Add(30 * time.Second)}, nil
		}), Target: routeTarget, Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = bridge.Serve(ctx, accessorServer) }()
		originEdge, origin := net.Pipe()
		defer origin.Close()
		connectorDone := make(chan error, 1)
		connectorReady := make(chan error, 1)
		go func() {
			stream, open, err := durableClient.AcceptStream(ctx)
			if err != nil {
				connectorReady <- err
				connectorDone <- err
				return
			}
			if open.Kind != "tcp_private" || open.RouteID != rule.RouteID {
				err = errors.New("wrong durable stream")
				connectorReady <- err
				connectorDone <- err
				return
			}
			connectorReady <- nil
			payload := make([]byte, len("client-to-origin"))
			if _, err = io.ReadFull(stream, payload); err == nil {
				_, err = originEdge.Write(payload)
			}
			if err == nil {
				payload = make([]byte, len("origin-to-client"))
				_, err = io.ReadFull(originEdge, payload)
				if err == nil {
					_, err = stream.Write(payload)
				}
			}
			connectorDone <- err
		}()
		stream, err := accessorClient.OpenStream(ctx, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: request.AccountID, TunnelID: accessorIdentity.TunnelID, ConnectorID: accessorIdentity.ConnectorID, SessionID: request.CarrierSessionID, ProcessGeneration: request.ProcessGeneration, Generation: request.ConfigGeneration, RouteID: request.RouteID, RequestID: request.RequestID, Kind: connectorprotocol.PrivateAccessTCP})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if err := connectorprotocol.WritePrivateAccessOpen(stream, connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "grant", Request: request}); err != nil {
			t.Fatal(err)
		}
		result, err := connectorprotocol.ReadPrivateAccessResult(stream, now)
		if err != nil || result.Status != http.StatusOK {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if err := <-connectorReady; err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.WriteString(stream, "client-to-origin") }()
		got := make([]byte, len("client-to-origin"))
		if _, err := io.ReadFull(origin, got); err != nil || string(got) != "client-to-origin" {
			t.Fatalf("origin got=%q err=%v", got, err)
		}
		go func() { _, _ = io.WriteString(origin, "origin-to-client") }()
		got = make([]byte, len("origin-to-client"))
		if _, err := io.ReadFull(stream, got); err != nil || string(got) != "origin-to-client" {
			t.Fatalf("client got=%q err=%v", got, err)
		}
		cancel()
		_ = originEdge.Close()
		select {
		case <-connectorDone:
		case <-time.After(time.Second):
			t.Fatal("durable bridge did not stop")
		}
	})

	for _, tc := range []struct {
		name        string
		mutate      func(*connectorprotocol.PrivateAccessRequest)
		allowed     bool
		withCarrier bool
		want        int
	}{{"wrong account", func(r *connectorprotocol.PrivateAccessRequest) { r.AccountID = "account_other" }, false, true, http.StatusForbidden}, {"stale assignment", func(r *connectorprotocol.PrivateAccessRequest) { r.AssignmentGeneration++ }, true, true, http.StatusServiceUnavailable}, {"removed carrier", func(*connectorprotocol.PrivateAccessRequest) {}, true, false, http.StatusServiceUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			accessorServer, accessorClient := testEdgePreviewCarrierPair(t, accessorIdentity)
			registry, _ := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2})
			defer registry.Close()
			var opened atomic.Int32
			var negativeDurableServer *datacarrier.Server
			if tc.withCarrier {
				durableServer, durableClient := testEdgePreviewCarrierPair(t, durableIdentity)
				negativeDurableServer = durableServer
				state := replicaState(rule, durableIdentity.Generation, rule.AssignmentID, "zone_1", time.Millisecond, 2)
				if err := registry.AttachReplica(durableServer, "owner-key", "owner-thumb", state); err != nil {
					t.Fatal(err)
				}
				acceptCtx, stopAccept := context.WithCancel(context.Background())
				defer stopAccept()
				go func() {
					stream, _, err := durableClient.AcceptStream(acceptCtx)
					if err == nil {
						opened.Add(1)
						_ = stream.Close()
					}
				}()
			}
			targetRules := privateTCPRouteSnapshot{rules: []route.RouteRule{rule}}
			bridge, _ := NewPrivateAccessStreamBridge(PrivateAccessStreamBridgeConfig{Authorizer: privateAccessGrantAuthorizerFunc(func(context.Context, string, connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
				if !tc.allowed {
					return control.PrivateAccessGrantDecision{Allowed: false}, nil
				}
				return control.PrivateAccessGrantDecision{Allowed: true, ExpiresAt: now.Add(30 * time.Second)}, nil
			}), Target: PrivateAccessTargetFunc(func(ctx context.Context, got connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
				return (PrivateAccessRouteTarget{Routes: targetRules, Carriers: registry}).OpenPrivateAccessTarget(ctx, got)
			}), Clock: func() time.Time { return now }})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = bridge.Serve(ctx, accessorServer) }()
			gotRequest := request
			tc.mutate(&gotRequest)
			metadata := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: gotRequest.AccountID, TunnelID: accessorIdentity.TunnelID, ConnectorID: accessorIdentity.ConnectorID, SessionID: gotRequest.CarrierSessionID, ProcessGeneration: gotRequest.ProcessGeneration, Generation: gotRequest.ConfigGeneration, RouteID: gotRequest.RouteID, RequestID: gotRequest.RequestID, Kind: connectorprotocol.PrivateAccessTCP}
			// Wrong account is rejected by policy while retaining the authenticated
			// carrier metadata, so metadata itself cannot become authority.
			if tc.name == "wrong account" {
				metadata.AccountID = accessorIdentity.AccountID
				gotRequest.AccountID = accessorIdentity.AccountID
			}
			stream, err := accessorClient.OpenStream(ctx, metadata)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if err := connectorprotocol.WritePrivateAccessOpen(stream, connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "grant", Request: gotRequest}); err != nil {
				t.Fatal(err)
			}
			result, err := connectorprotocol.ReadPrivateAccessResult(stream, now)
			if err != nil || result.Status != tc.want {
				t.Fatalf("result=%+v err=%v want=%d", result, err, tc.want)
			}
			if opened.Load() != 0 || negativeDurableServer != nil && negativeDurableServer.ActiveStreams() != 0 {
				t.Fatalf("origin target opened=%d", opened.Load())
			}
		})
	}
}

func edgePrivateAccessRequest(now time.Time) connectorprotocol.PrivateAccessRequest {
	return connectorprotocol.PrivateAccessRequest{
		AccountID: "account_1", ResourceKind: "preview", ResourceID: "preview_1", RouteID: "route_1",
		Audience: "paperboat-preview-http", MachineID: "machine_1", SessionID: "installation_1", InstallationGeneration: 1,
		ExpiresAt: now.Add(time.Minute), Nonce: "nonce_1", OperationID: "operation_1", CarrierSessionID: "session_1",
		RouteGeneration: 1, ProcessGeneration: 2, ConfigGeneration: 3, SessionGeneration: 4, AssignmentGeneration: 5,
		EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", Protocol: "http", Method: http.MethodConnect,
		Host: "private.preview.example.test", Path: "/", IdempotencyKey: "access_1", RequestID: "request_1", CorrelationID: "correlation_1",
	}
}

func TestPrivateAccessStreamBridgeAuthorizesBeforeOpaqueDuplex(t *testing.T) {
	now := time.Now().UTC()
	identity := testEdgePreviewIdentity(2, 3)
	server, client := testEdgePreviewCarrierPair(t, identity)
	request := edgePrivateAccessRequest(now)
	request.AccountID = identity.AccountID
	request.MachineID = identity.HostID
	request.CarrierSessionID = identity.SessionID
	request.ProcessGeneration = identity.ProcessGeneration
	request.ConfigGeneration = identity.Generation
	targetEdge, targetOrigin := net.Pipe()
	defer targetOrigin.Close()
	authorizer := privateAccessGrantAuthorizerFunc(func(_ context.Context, grant string, got connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
		if grant != "signed-grant" || got != request {
			t.Fatalf("grant=%q request=%+v", grant, got)
		}
		return control.PrivateAccessGrantDecision{Allowed: true, ExpiresAt: now.Add(30 * time.Second)}, nil
	})
	bridge, err := NewPrivateAccessStreamBridge(PrivateAccessStreamBridgeConfig{Authorizer: authorizer, Target: PrivateAccessTargetFunc(func(context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
		return targetEdge, nil
	}), Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- bridge.Serve(runCtx, server) }()
	metadata := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: request.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: request.CarrierSessionID, ProcessGeneration: request.ProcessGeneration, Generation: request.ConfigGeneration, RouteID: request.RouteID, RequestID: request.RequestID, Kind: connectorprotocol.PrivateAccessHTTP}
	stream, err := client.OpenStream(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := connectorprotocol.WritePrivateAccessOpen(stream, connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed-grant", Request: request}); err != nil {
		t.Fatal(err)
	}
	result, err := connectorprotocol.ReadPrivateAccessResult(stream, now)
	if err != nil || result.Status != http.StatusOK {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	writeDone := make(chan error, 1)
	go func() { _, err := io.WriteString(stream, "browser-tls"); writeDone <- err }()
	got := make([]byte, len("browser-tls"))
	if _, err := io.ReadFull(targetOrigin, got); err != nil || string(got) != "browser-tls" {
		t.Fatalf("target bytes=%q err=%v", got, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = server.Close()
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop")
	}
}

func TestPrivateAccessStreamBridgeMapsAuthorizationFailures(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{{control.ErrPrivateAccessGrantUnauthorized, http.StatusUnauthorized}, {control.ErrPrivateAccessGrantForbidden, http.StatusForbidden}, {control.ErrPrivateAccessGrantUnavailable, http.StatusServiceUnavailable}} {
		if got := privateAccessErrorStatus(test.err); got != test.status {
			t.Fatalf("error %v status=%d want=%d", test.err, got, test.status)
		}
	}
	if !errors.Is(ErrPrivateAccessStreamInvalid, ErrPrivateAccessStreamInvalid) {
		t.Fatal("sentinel mismatch")
	}
}

func TestPrivateTCPRuleRequiresExactDurableGenerationTuple(t *testing.T) {
	request := edgePrivateAccessRequest(time.Now().UTC())
	request.ResourceKind = "tunnel"
	request.ResourceID = "tunnel_1"
	request.Audience = "paperboat-tunnel-tcp"
	request.Protocol = "tcp"
	request.Method, request.Host, request.Path = "", "", ""
	rule := route.RouteRule{RouteID: request.RouteID, RouteGeneration: request.RouteGeneration, AssignmentGeneration: request.AssignmentGeneration, ResourceKind: "tunnel", SessionGeneration: request.SessionGeneration, AccountID: request.AccountID, TunnelID: request.ResourceID, ConnectorID: request.ConnectorID, ConnectorSessionID: request.CarrierSessionID, ConnectorProcessGeneration: request.ProcessGeneration, ConfigGeneration: request.ConfigGeneration, Node: request.EdgeNodeID, EdgeProcessEpoch: request.EdgeProcessEpoch, Kind: route.TunnelPrivateTCP, Protocol: "private_tcp", AccessMode: "private"}
	if got, err := privateTCPRule([]route.RouteRule{rule}, request); err != nil || got.RouteID != rule.RouteID {
		t.Fatalf("exact route got=%+v err=%v", got, err)
	}
	stale := request
	stale.AssignmentGeneration++
	if _, err := privateTCPRule([]route.RouteRule{rule}, stale); !errors.Is(err, ErrPrivateAccessStreamInvalid) {
		t.Fatalf("stale assignment error=%v", err)
	}
	if _, err := privateTCPRule([]route.RouteRule{rule, rule}, request); !errors.Is(err, ErrPrivateAccessStreamInvalid) {
		t.Fatalf("duplicate route error=%v", err)
	}
}

type privateTLSStreamConn struct{ *datacarrier.Stream }

func (c privateTLSStreamConn) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (c privateTLSStreamConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }

func TestPrivatePreviewGrantReachesTLSHTTPIngress(t *testing.T) {
	now := time.Now().UTC()
	identity := testEdgePreviewIdentity(2, 3)
	id := func(noun string) string { return noun + "_11111111-1111-4111-8111-111111111111" }
	identity.AccountID, identity.TunnelID, identity.ConnectorID, identity.HostID, identity.SessionID = id("user"), id("machine"), id("machine"), id("machine"), id("session")
	server, client := testEdgePreviewCarrierPair(t, identity)
	request := edgePrivateAccessRequest(now)
	request.AccountID, request.MachineID, request.CarrierSessionID = identity.AccountID, identity.HostID, identity.SessionID
	request.ResourceID, request.RouteID, request.OperationID = id("preview"), id("route"), id("operation")
	request.SessionID, request.IdempotencyKey, request.RequestID, request.CorrelationID = id("session"), id("operation"), id("request"), id("correlation")
	request.ProcessGeneration, request.ConfigGeneration = identity.ProcessGeneration, identity.Generation
	request.SessionGeneration, request.AssignmentGeneration = 1, 1
	request.EdgeProcessEpoch = "epoch_private_preview_12345678"
	preview := DataCarrierPreviewRoute{Identity: identity, RouteID: request.RouteID, Revision: request.RouteGeneration, PreviewID: request.ResourceID, OperationID: request.OperationID, LeaseGeneration: 1, AttachmentGeneration: 1, EdgeNodeID: request.EdgeNodeID, EdgeProcessEpoch: request.EdgeProcessEpoch, Kind: dataCarrierPreviewPrivateRouteKind, AccessMode: "private", Hostname: request.Host, Endpoint: "https://" + request.Host}
	preview.Server, preview.OwnerMachineID, preview.OwnerSessionID = server, identity.HostID, id("session")
	preview.ConfigContentHash, preview.ExpiresAt = "sha256:"+strings.Repeat("a", 64), request.ExpiresAt
	preview.MachineIdentityPublicKey, preview.MachineIdentityThumbprint = "test-public-key", "test-thumbprint"
	published, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: request.EdgeProcessEpoch, MaximumRoutes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if err = published.Attach(preview); err != nil {
		t.Fatal(err)
	}
	_, decision := browserTestMatch()
	decision.Binding.Lifecycle = connectorprotocol.TunnelEphemeral
	decision.Binding.AccountID = request.AccountID
	decision.Binding.TunnelID = identity.TunnelID
	decision.Binding.HostID = identity.HostID
	decision.Binding.Hostname = request.Host
	decision.Binding.PublicationID = request.ResourceID
	decision.Binding.RouteID = request.RouteID
	decision.Binding.RouteGeneration = request.RouteGeneration
	decision.Binding.TargetID = request.RouteID
	decision.Binding.TargetGeneration = request.RouteGeneration
	decision.ConnectorID = identity.ConnectorID
	decision.SessionID = identity.SessionID
	decision.ProcessGeneration = identity.ProcessGeneration
	decision.ConfigGeneration = identity.Generation
	decision.AssignmentGeneration = request.AssignmentGeneration
	decision.EdgeNodeID = request.EdgeNodeID
	decision.EdgeProcessEpoch = request.EdgeProcessEpoch
	decision.PrincipalID = request.MachineID
	decision.GrantID = request.Nonce
	decision.GrantGeneration = request.InstallationGeneration
	decision.MembershipGeneration = 0
	decision.IssuedAt = now
	decision.ExpiresAt = now.Add(10 * time.Second)
	decision.NativeAuthorization = &connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "signed-grant", Request: request}
	if err := decision.Validate(now); err != nil {
		t.Fatal(err)
	}
	recorder := &ingressUsageRecorder{}
	metering, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 1, IngressLimits: testIngressLimits(1, 1<<20), Usage: recorder})
	if err != nil {
		t.Fatal(err)
	}
	defer metering.Close()
	transport, err := NewDataCarrierPreviewTransport(DataCarrierPreviewTransportConfig{Registry: published, IngressRegistry: metering})
	if err != nil {
		t.Fatal(err)
	}
	originDone := make(chan error, 2)
	go func() {
		for i := 0; i < 2; i++ {
			stream, open, err := client.AcceptStream(context.Background())
			if err != nil {
				originDone <- err
				return
			}
			defer stream.Close()
			if open.Kind != "https" || open.RouteID != request.RouteID || open.SessionID != identity.SessionID {
				originDone <- fmt.Errorf("incorrect private origin stream binding")
				return
			}
			originDone <- datacarrier.ServeHTTPStream(context.Background(), stream, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "private origin reached") }), 4096, time.Second)
		}
	}()
	var revoked atomic.Bool
	var reauthorizations atomic.Int32
	transport.privateAuthority = func(_ context.Context, evidence connectorprotocol.PrivateAccessOpen) (connectorprotocol.IngressDecision, error) {
		reauthorizations.Add(1)
		if revoked.Load() || evidence.Request != request || evidence.Grant != "signed-grant" {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		fresh := decision
		fresh.IssuedAt = time.Now().UTC()
		fresh.ExpiresAt = fresh.IssuedAt.Add(4 * time.Second)
		return fresh, nil
	}
	registry, _ := NewPrivateAccessConnectionRegistry(8)
	policy, err := New(Config{SelfHosted: true, MaxHeaderBytes: 4096, MaxBodyBytes: 1024, Routes: NewPreviewCarrierRouteMatcher(published), PrivateAccessConnections: registry}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := transport.RoundTrip(r)
		if err != nil {
			if !revoked.Load() {
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
	go bridge.Serve(ctx, server)
	stream, err := client.OpenStream(ctx, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: request.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: request.CarrierSessionID, ProcessGeneration: request.ProcessGeneration, Generation: request.ConfigGeneration, RouteID: request.RouteID, RequestID: request.RequestID, Kind: connectorprotocol.PrivateAccessHTTP})
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
	if _, err = io.WriteString(secured, "GET / HTTP/1.1\r\nHost: "+request.Host+"\r\nConnection: keep-alive\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(secured), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "private origin reached" {
		t.Fatalf("HTTP status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	response.Body.Close()
	// The same admitted TLS connection retains proof after its short ingress
	// snapshot expires; every new request must obtain current server authority.
	registry.mu.Lock()
	for address, entry := range registry.entries {
		expired := *entry.ingress
		expired.IssuedAt = time.Now().Add(-11 * time.Second)
		expired.ExpiresAt = expired.IssuedAt.Add(10 * time.Second)
		entry.ingress = &expired
		registry.entries[address] = entry
	}
	registry.mu.Unlock()
	if _, err = io.WriteString(secured, "GET / HTTP/1.1\r\nHost: "+request.Host+"\r\nConnection: keep-alive\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	second, err := http.ReadResponse(bufio.NewReader(secured), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(second.Body)
	second.Body.Close()
	if err != nil || second.StatusCode != 200 || string(body) != "private origin reached" {
		t.Fatalf("persistent native preview failed status=%d body=%q err=%v", second.StatusCode, body, err)
	}
	for i := 0; i < 2; i++ {
		if err := <-originDone; err != nil {
			t.Fatal(err)
		}
	}

	recorder.mu.Lock()
	var upload, download uint64
	for _, r := range recorder.records {
		if r.route != request.RouteID || r.revision != request.RouteGeneration {
			t.Fatal("wrong native preview accounting binding")
		}
		upload += r.ingress
		download += r.egress
	}
	recorder.mu.Unlock()
	if upload == 0 || download < uint64(len("private origin reached")) {
		t.Fatalf("native preview unmetered upload=%d download=%d", upload, download)
	}
	lease, err := metering.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatalf("native response retained capacity: %v", err)
	}
	lease.Release()
	missing := httptest.NewRequest("GET", "https://"+request.Host+"/", nil)
	missing = missing.WithContext(context.WithValue(missing.Context(), privateAccessRequestContextKey{}, request))
	if response, err := transport.RoundTrip(missing); err == nil {
		response.Body.Close()
		t.Fatal("native preview admitted without accounting authority")
	}

	revoked.Store(true)
	if _, err = io.WriteString(secured, "GET / HTTP/1.1\r\nHost: "+request.Host+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	denied, err := http.ReadResponse(bufio.NewReader(secured), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, denied.Body)
	denied.Body.Close()
	if denied.StatusCode != 502 || reauthorizations.Load() != 3 {
		t.Fatalf("revoked request status=%d authorization calls=%d", denied.StatusCode, reauthorizations.Load())
	}
	recorder.mu.Lock()
	var afterUpload, afterDownload uint64
	for _, r := range recorder.records {
		afterUpload += r.ingress
		afterDownload += r.egress
	}
	recorder.mu.Unlock()
	if afterUpload != upload || afterDownload != download {
		t.Fatal("revoked request recorded application bytes")
	}

	for name, mutate := range map[string]func(*connectorprotocol.PrivateAccessRequest){
		"operation": func(q *connectorprotocol.PrivateAccessRequest) { q.OperationID = id("other") },
		"session":   func(q *connectorprotocol.PrivateAccessRequest) { q.CarrierSessionID = id("other") },
		"expired":   func(q *connectorprotocol.PrivateAccessRequest) { q.ExpiresAt = now.Add(-time.Second) },
		"account":   func(q *connectorprotocol.PrivateAccessRequest) { q.AccountID = id("other") },
	} {
		invalid := request
		mutate(&invalid)
		originRequest := httptest.NewRequest(http.MethodGet, "https://"+request.Host+"/", nil)
		originRequest = originRequest.WithContext(context.WithValue(originRequest.Context(), privateAccessRequestContextKey{}, invalid))
		if _, err := transport.RoundTrip(originRequest); !errors.Is(err, ErrDataCarrierPreviewTransport) {
			t.Fatalf("%s invalid grant transport error=%v", name, err)
		}
	}
	// An unregistered public connection cannot gain this grant using headers.
	publicReq := httptest.NewRequest(http.MethodGet, "https://"+request.Host+"/", nil)
	publicReq.Header.Set("X-Paperboat-Private-Carrier", "forged")
	publicReq.Header.Set("X-Paperboat-Private-Connection", tlsServer.Listener.Addr().String())
	publicResponse := httptest.NewRecorder()
	policy.ServeHTTP(publicResponse, publicReq)
	if publicResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unregistered public request status=%d", publicResponse.Code)
	}
}

func TestPrivateAccessTargetFailureRecoveryAndJoinedShutdown(t *testing.T) {
	now := time.Now().UTC()
	identity := testEdgePreviewIdentity(2, 3)
	server, client := testEdgePreviewCarrierPair(t, identity)
	request := edgePrivateAccessRequest(now)
	request.AccountID, request.MachineID, request.CarrierSessionID = identity.AccountID, identity.HostID, identity.SessionID
	request.ProcessGeneration, request.ConfigGeneration = identity.ProcessGeneration, identity.Generation
	target, origin := net.Pipe()
	defer origin.Close()
	failures := make(chan error, 4)
	var opens atomic.Int32
	bridge, err := NewPrivateAccessStreamBridge(PrivateAccessStreamBridgeConfig{
		Authorizer: privateAccessGrantAuthorizerFunc(func(context.Context, string, connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
			return control.PrivateAccessGrantDecision{Allowed: true, ExpiresAt: now.Add(time.Minute)}, nil
		}),
		Target: PrivateAccessTargetFunc(func(context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
			if opens.Add(1) == 1 {
				return nil, syscall.EIO
			}
			return target, nil
		}),
		OnFailure: func(ctx context.Context, phase string, cause error) { failures <- cause },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bridge.Serve(ctx, server) }()
	open := func(id string, want int) *datacarrier.Stream {
		r := request
		r.RequestID = id
		stream, err := client.OpenStream(ctx, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: r.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: r.CarrierSessionID, ProcessGeneration: r.ProcessGeneration, Generation: r.ConfigGeneration, RouteID: r.RouteID, RequestID: id, Kind: connectorprotocol.PrivateAccessHTTP})
		if err != nil {
			t.Fatal(err)
		}
		if err := connectorprotocol.WritePrivateAccessOpen(stream, connectorprotocol.PrivateAccessOpen{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Grant: "private-grant", Request: r}); err != nil {
			t.Fatal(err)
		}
		result, err := connectorprotocol.ReadPrivateAccessResult(stream, now)
		if err != nil || result.Status != want {
			t.Fatalf("unexpected access result: %d cause %T", result.Status, err)
		}
		return stream
	}
	failed := open("request_failed", http.StatusServiceUnavailable)
	failed.Close()
	select {
	case err := <-failures:
		if !errors.Is(err, syscall.EIO) {
			t.Fatal("lost open cause")
		}
	case <-time.After(time.Second):
		t.Fatal("missing open diagnostic")
	}
	recovered := open("request_recovered", http.StatusOK)
	defer recovered.Close()
	sent := make(chan error, 1)
	go func() { _, err := io.WriteString(recovered, "opaque"); sent <- err }()
	payload := make([]byte, 6)
	if _, err := io.ReadFull(origin, payload); err != nil || string(payload) != "opaque" {
		t.Fatal("target did not recover")
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("accepted stream workers did not join")
	}
	select {
	case err := <-failures:
		var types []string
		requestErrorLeaves(err, func(leaf error) bool {
			label := fmt.Sprintf("%T", leaf)
			if leaf == io.ErrClosedPipe {
				label = "closed_pipe"
			}
			if leaf == context.Canceled {
				label = "canceled"
			}
			if leaf == io.EOF {
				label = "eof"
			}
			if leaf == yamux.ErrStreamClosed {
				label = "stream_closed"
			}
			if leaf == yamux.ErrStreamReset {
				label = "stream_reset"
			}
			if leaf == yamux.ErrTimeout {
				label = "timeout"
			}
			types = append(types, label)
			return false
		}, false)
		t.Fatalf("normal shutdown produced extra failure: %v", types)
	default:
	}
}

func TestPrivateAccessObserverPreservesMixedCausesAndSkipsOwnedAttempt(t *testing.T) {
	var got []error
	b := &PrivateAccessStreamBridge{onFailure: func(_ context.Context, _ string, err error) { got = append(got, err) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.observe(ctx, "private_stream_copy", errors.Join(context.Canceled, io.EOF))
	attempt := &control.RequestFailure{Status: 503, Cause: syscall.ECONNREFUSED}
	b.observe(ctx, "private_stream_authorize", fmt.Errorf("%w", attempt))
	b.observe(ctx, "private_stream_authorize", errors.Join(attempt, syscall.EIO))
	b.observe(ctx, "private_stream_copy", errors.Join(context.Canceled, syscall.EIO))
	if len(got) != 2 || !errors.Is(got[0], syscall.EIO) || !errors.Is(got[1], syscall.EIO) {
		t.Fatal("mixed failure policy")
	}
}

func TestPrivateAccessShutdownJoinsBlockedOpenEnvelope(t *testing.T) {
	identity := testEdgePreviewIdentity(2, 3)
	server, client := testEdgePreviewCarrierPair(t, identity)
	bridge, err := NewPrivateAccessStreamBridge(PrivateAccessStreamBridgeConfig{
		Authorizer: privateAccessGrantAuthorizerFunc(func(context.Context, string, connectorprotocol.PrivateAccessRequest) (control.PrivateAccessGrantDecision, error) {
			t.Error("incomplete request reached authorization")
			return control.PrivateAccessGrantDecision{}, nil
		}),
		Target: PrivateAccessTargetFunc(func(context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
			t.Error("incomplete request reached target")
			return nil, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	stream, err := client.OpenStream(ctx, connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, Generation: identity.Generation, RouteID: "route_incomplete", RequestID: "request_incomplete", Kind: connectorprotocol.PrivateAccessHTTP})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	accepted, metadata, err := server.AcceptAccessStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- bridge.serveStream(ctx, accepted, metadata, identity) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked envelope prevented shutdown")
	}
	if server.ActiveStreams() != 0 {
		t.Fatal("accepted stream permit leaked")
	}
}
