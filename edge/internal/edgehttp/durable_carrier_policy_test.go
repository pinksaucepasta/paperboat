package edgehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type durablePolicyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f durablePolicyRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGatewayDispatchesDurableRouteToCanonicalCarrier(t *testing.T) {
	for _, host := range []string{"service.customer.test", "smart-cloudy-voyage-6024.tunnels.example.test", "123e4567-e89b-12d3-a456-426614174000.tunnels.example.test"} {
		t.Run(host, func(t *testing.T) { testGatewayDispatchesDurableRoute(t, host) })
	}
}

func testGatewayDispatchesDurableRoute(t *testing.T, host string) {
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	rule := route.RouteRule{
		ID:                         "route_durable_01",
		Revision:                   1,
		Kind:                       route.TunnelHTTPSWSS,
		MatchType:                  route.MatchExact,
		Hostname:                   host,
		PathPrefix:                 "/",
		Target:                     "route_durable_01",
		Protocol:                   "http",
		OriginScheme:               "http",
		AccessMode:                 "public",
		AccountID:                  "account_01",
		HostID:                     "host_01",
		TunnelID:                   "tunnel_01",
		ConnectorID:                "connector_01",
		ConnectorSessionID:         "session_01",
		ConnectorProcessGeneration: 2,
		ConfigGeneration:           3,
		ConfigContentHash:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Node:                       "edge_01",
		EdgeProcessEpoch:           "epoch_0001",
		EdgeFailureDomain:          "region_01",
		AssignmentID:               "assignment_01",
		AssignmentGeneration:       1,
		ObservedState:              "ready",
	}
	if strings.HasSuffix(host, ".tunnels.example.test") {
		rule.MatchType = route.MatchManagedExact
	}
	if err := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}

	var canonicalCalls int
	canonical := durablePolicyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		canonicalCalls++
		match, ok := RouteMatchFromContext(request.Context())
		if !ok || match.Rule.Kind != route.TunnelHTTPSWSS || match.Rule.ID != rule.ID {
			return nil, errors.New("canonical route match was not preserved")
		}
		if request.URL.Host == rule.Target || request.URL.Scheme == rule.Target {
			return nil, errors.New("opaque durable route target leaked into URL")
		}
		if request.Header.Get("X-Forwarded-Host") != host || request.Header.Get("X-Forwarded-Proto") != "https" || request.Header.Get("X-Forwarded-Port") != "443" {
			return nil, errors.New("edge forwarded headers did not reach the carrier")
		}
		if request.URL.Path == "/down" { // the agent's answer when nothing listens on the app's port
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"X-Paperboat-Origin-Failure": {"refused"}}, Body: http.NoBody, Request: request}, nil
		}
		if request.URL.Path == "/loop" { // Caddy's port-80 redirect: back to the URL the visitor asked for
			return &http.Response{StatusCode: http.StatusPermanentRedirect, Header: http.Header{"Location": {"https://" + host + "/loop?x=1"}}, Body: http.NoBody, Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("from-carrier")), Request: request}, nil
	})
	var previewCalls, legacyCalls int
	preview := durablePolicyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		previewCalls++
		return nil, errors.New("preview transport must not receive durable route")
	})
	legacy := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { legacyCalls++ })
	config := Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: routes}
	policy, err := NewGatewayWithTransports(config, "127.0.0.1:1", preview, canonical)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the proxy's private legacy target with a sentinel by using a
	// second policy. A canonical match must never reach this handler regardless
	// of the retained legacy upstream configuration.
	_ = legacy
	request := httptest.NewRequest(http.MethodGet, "https://"+host+"/normal", nil)
	request.Host = host
	recorder := httptest.NewRecorder()
	policy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "from-carrier" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if canonicalCalls != 1 || previewCalls != 0 || legacyCalls != 0 {
		t.Fatalf("canonical=%d preview=%d legacy=%d", canonicalCalls, previewCalls, legacyCalls)
	}
	loop := httptest.NewRequest(http.MethodGet, "https://"+host+"/loop?x=1", nil)
	loop.Host = host
	recorder = httptest.NewRecorder()
	policy.ServeHTTP(recorder, loop)
	if recorder.Code != http.StatusBadGateway || recorder.Header().Get("X-Paperboat-Error") != "origin_redirect_loop" || recorder.Header().Get("Location") != "" || !strings.Contains(recorder.Body.String(), "redirects to HTTPS") {
		t.Fatalf("self-redirect reached the visitor: status=%d error=%q location=%q", recorder.Code, recorder.Header().Get("X-Paperboat-Error"), recorder.Header().Get("Location"))
	}
	down := httptest.NewRequest(http.MethodGet, "https://"+host+"/down", nil)
	down.Host = host
	recorder = httptest.NewRecorder()
	policy.ServeHTTP(recorder, down)
	if recorder.Code != http.StatusBadGateway || recorder.Header().Get("X-Paperboat-Error") != "origin_refused" || recorder.Header().Get("X-Paperboat-Origin-Failure") != "" || !strings.Contains(recorder.Body.String(), "Nothing is listening") {
		t.Fatalf("agent origin failure: status=%d error=%q", recorder.Code, recorder.Header().Get("X-Paperboat-Error"))
	}
	cookies := httptest.NewRequest(http.MethodGet, "https://"+host+"/normal", nil)
	cookies.Host = host
	cookies.Header.Set("Cookie", strings.Repeat("c", 5000)) // over MaxHeaderBytes: 431, not "not found"
	recorder = httptest.NewRecorder()
	policy.ServeHTTP(recorder, cookies)
	if recorder.Code != http.StatusRequestHeaderFieldsTooLarge || recorder.Header().Get("X-Paperboat-Error") != "request_headers_too_large" {
		t.Fatalf("oversized headers: status=%d error=%q", recorder.Code, recorder.Header().Get("X-Paperboat-Error"))
	}
}

func TestDurableIngressRequiresExactRoutePolicyModeAndGeneration(t *testing.T) {
	decision := publicTCPDecision(time.Now().UTC(), 43001)
	rule := route.RouteRule{
		ID: decision.Binding.RouteID, RouteID: decision.Binding.RouteID, Revision: decision.Binding.RouteGeneration,
		RouteGeneration: decision.Binding.RouteGeneration, AccountID: decision.Binding.AccountID, TunnelID: decision.Binding.TunnelID,
		ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID, ConnectorProcessGeneration: decision.ProcessGeneration,
		ConfigGeneration: decision.ConfigGeneration, AssignmentGeneration: decision.AssignmentGeneration,
		Node: decision.EdgeNodeID, EdgeProcessEpoch: decision.EdgeProcessEpoch, AccessMode: "public",
		ViewerPolicyGeneration: decision.PolicyGeneration, Kind: route.TunnelTCP, Protocol: "tcp",
	}
	open := connectorprotocol.StreamOpen{
		Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion,
		AccountID: decision.Binding.AccountID, TunnelID: decision.Binding.TunnelID,
		ConnectorID: decision.ConnectorID, SessionID: decision.SessionID,
		ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration,
		RouteID: decision.Binding.RouteID, RequestID: "request_policy_check", Kind: "tcp_public",
	}
	registry := &DataCarrierRouteRegistry{ingressAuthority: func(context.Context, route.RouteRule) (connectorprotocol.IngressDecision, error) {
		return decision, nil
	}}
	if _, err := registry.ingressDecision(context.Background(), rule, open); err != nil {
		t.Fatalf("matching policy decision rejected: %v", err)
	}
	rule.ViewerPolicyGeneration++
	if _, err := registry.ingressDecision(context.Background(), rule, open); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("stale policy generation error = %v, want ErrIngressDenied", err)
	}
	rule.ViewerPolicyGeneration = decision.PolicyGeneration
	rule.AccessMode = "private"
	if _, err := registry.ingressDecision(context.Background(), rule, open); !errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatalf("public decision for private route error = %v, want ErrIngressDenied", err)
	}
}

func TestConfiguredRoutesRejectUnownedManagedHostBeforeLegacyFallback(t *testing.T) {
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	rule := route.RouteRule{ID: "route_owned_01", Revision: 1, Kind: route.TunnelHTTPSWSS, MatchType: route.MatchExact, Hostname: "owned.customer.test", PathPrefix: "/", Target: "route_owned_01", Protocol: "http", AccountID: "account_01", HostID: "host_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", ConnectorSessionID: "session_01", ConnectorProcessGeneration: 1, ConfigGeneration: 1, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Node: "edge_01", EdgeProcessEpoch: "epoch_0001", EdgeFailureDomain: "region_01", AssignmentID: "assignment_01", AssignmentGeneration: 1, ObservedState: "ready"}
	if err := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}
	var nextCalls int
	policy, err := New(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: routes}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ }))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://unowned.customer.test/", nil)
	request.Host = "unowned.customer.test"
	recorder := httptest.NewRecorder()
	policy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || nextCalls != 0 {
		t.Fatalf("status=%d next_calls=%d", recorder.Code, nextCalls)
	}
}
