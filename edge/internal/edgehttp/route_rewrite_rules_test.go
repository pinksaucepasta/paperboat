package edgehttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

// A tunnel route's rules run at the edge: a redirect answers without touching the carrier, a path rewrite and request
// headers change what the carrier sees, response headers change what the visitor sees, and nothing a rule says can
// override what the edge sets (forwarded headers, the visitor assertion, the app's security headers).
func TestTunnelRouteRulesRedirectRewriteAndHeadersAtTheEdge(t *testing.T) {
	const host = "rules.tunnels.example.test"
	// deliberately not normalized: the blocked headers are what a stale or hostile snapshot could carry, and the edge skips them
	rules := tunnelrouterules.Rules{
		RequestHeaders:  []tunnelrouterules.HeaderRule{{Op: "set", Name: "X-Route", Value: "api"}, {Op: "set", Name: "X-Forwarded-For", Value: "6.6.6.6"}, {Op: "remove", Name: "X-Real-IP"}},
		ResponseHeaders: []tunnelrouterules.HeaderRule{{Op: "set", Name: "X-Served-By", Value: "paperboat"}, {Op: "set", Name: "X-Frame-Options", Value: "ALLOWALL"}, {Op: "remove", Name: "X-Secret"}},
		PathRewrite:     &tunnelrouterules.PathRewrite{From: "/api", To: "/v1"},
		Redirects:       []tunnelrouterules.Redirect{{From: "/old", Code: 308, To: "/new"}},
	}
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	rule := route.RouteRule{
		ID: "route_rules_01", Revision: 1, Kind: route.TunnelHTTPSWSS, MatchType: route.MatchManagedExact, Hostname: host, PathPrefix: "/", Target: "route_rules_01",
		Protocol: "http", OriginScheme: "http", AccessMode: "public", AccountID: "account_01", HostID: "host_01", TunnelID: "tunnel_01", ConnectorID: "connector_01",
		ConnectorSessionID: "session_01", ConnectorProcessGeneration: 2, ConfigGeneration: 3,
		ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Node:              "edge_01", EdgeProcessEpoch: "epoch_0001", EdgeFailureDomain: "region_01", AssignmentID: "assignment_01", AssignmentGeneration: 1,
		ObservedState: "ready", RewriteRules: &rules,
	}
	if err := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}
	var calls int
	var seen *http.Request
	canonical := durablePolicyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		seen = request
		header := http.Header{"X-Secret": {"s3"}, "X-Frame-Options": {"SAMEORIGIN"}}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	policy, err := NewGatewayWithTransports(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: routes},
		"127.0.0.1:1", durablePolicyRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.EOF }), canonical)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, target string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "https://"+host+target, nil)
		request.Host, request.RemoteAddr = host, "198.51.100.7:4000"
		if mutate != nil {
			mutate(request)
		}
		recorder := httptest.NewRecorder()
		policy.ServeHTTP(recorder, request)
		return recorder
	}
	// a redirect never reaches the carrier, keeps the rest of the path and the query, and HEAD has no body
	if got := serve(http.MethodGet, "/old/page?a=1", nil); got.Code != 308 || got.Header().Get("Location") != "/new/page?a=1" || calls != 0 {
		t.Fatalf("redirect: %d %q carrier calls %d", got.Code, got.Header().Get("Location"), calls)
	}
	if head := serve(http.MethodHead, "/old", nil); head.Code != 308 || head.Body.Len() != 0 {
		t.Fatalf("HEAD redirect: %d body %q", head.Code, head.Body.String())
	}
	// everything else goes to the carrier with the rewritten path and headers, and the edge's own facts intact
	got := serve(http.MethodGet, "/api/users?x=1", func(r *http.Request) {
		r.Header.Set("X-Real-IP", "9.9.9.9")
		r.Header.Set("X-Forwarded-For", "8.8.8.8")
	})
	if got.Code != http.StatusOK || calls != 1 || seen.URL.Path != "/v1/users" || seen.URL.RawQuery != "x=1" {
		t.Fatalf("rewrite: status %d calls %d path %q", got.Code, calls, seen.URL.Path)
	}
	if seen.Header.Get("X-Route") != "api" || seen.Header.Get("X-Forwarded-For") != "198.51.100.7" || seen.Header.Get("X-Real-Ip") != "198.51.100.7" || seen.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("request headers at the carrier: %v", seen.Header)
	}
	if got.Header().Get("X-Served-By") != "paperboat" || got.Header().Get("X-Secret") != "" || got.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("response headers: %v", got.Header())
	}
	// a path the rules do not touch is not touched; a prefix match respects segment boundaries
	if serve(http.MethodGet, "/apix", nil); seen.URL.Path != "/apix" {
		t.Fatalf("/apix became %q", seen.URL.Path)
	}
	// what the visitor encoded stays encoded: %2F is not a path separator for the app, a redirect keeps the escaped rest
	serve(http.MethodGet, "/api/files/a%2Fb", nil)
	if seen.URL.EscapedPath() != "/v1/files/a%2Fb" || seen.URL.Path != "/v1/files/a/b" {
		t.Fatalf("encoded slash after the rewrite: escaped %q decoded %q", seen.URL.EscapedPath(), seen.URL.Path)
	}
	if got := serve(http.MethodGet, "/old/a%20b", nil); got.Header().Get("Location") != "/new/a%20b" {
		t.Fatalf("redirect of an escaped path: %q", got.Header().Get("Location"))
	}
	// a WebSocket handshake keeps its path (and is not redirected), but still gets the header rules
	serve(http.MethodGet, "/api/ws", func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	})
	if seen.URL.Path != "/api/ws" || seen.Header.Get("X-Route") != "api" {
		t.Fatalf("websocket handshake: path %q headers %v", seen.URL.Path, seen.Header)
	}
}
