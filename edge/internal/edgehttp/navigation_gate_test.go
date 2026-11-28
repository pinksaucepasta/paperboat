package edgehttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

// Only a browser page load is gated on the Free plan: a document or frame navigation, or an old client that sends no
// Sec-Fetch headers but asks for HTML. fetch/XHR, WebSocket upgrades, subresources and webhooks pass.
func TestPageLoadDetection(t *testing.T) {
	for _, c := range []struct {
		name    string
		method  string
		headers map[string]string
		want    bool
	}{
		{"document navigation", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, true},
		{"iframe navigation", "GET", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, true},
		{"form post navigation", "POST", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, true},
		{"no Sec-Fetch, Accept html", "GET", map[string]string{"Accept": "text/html,application/xhtml+xml"}, true},
		{"no Sec-Fetch, Accept HTML mixed case", "GET", map[string]string{"Accept": "Text/HTML"}, true},
		{"fetch", "GET", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty", "Accept": "text/html"}, false},
		{"xhr same-origin", "POST", map[string]string{"Sec-Fetch-Mode": "same-origin", "Sec-Fetch-Dest": "empty"}, false},
		{"script subresource", "GET", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}, false},
		{"image subresource", "GET", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image", "Accept": "image/*,text/html"}, false},
		{"object embed", "GET", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "object"}, true},
		{"embed", "GET", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "embed"}, true},
		{"websocket upgrade", "GET", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-Fetch-Mode": "websocket", "Sec-Fetch-Dest": "websocket"}, false},
		{"websocket upgrade, no Sec-Fetch, Accept html", "GET", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Accept": "text/html"}, false},
		{"webhook post", "POST", map[string]string{"Content-Type": "application/json", "Accept": "*/*"}, false},
		{"api client", "GET", map[string]string{"Accept": "application/json"}, false},
		{"curl default", "GET", map[string]string{"Accept": "*/*"}, false},
		{"no headers at all", "GET", nil, false},
	} {
		r := httptest.NewRequest(c.method, "https://app.example.test/", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := pageLoad(r); got != c.want {
			t.Errorf("%s: pageLoad = %v, want %v", c.name, got, c.want)
		}
	}
}

// A public tunnel route the server marked navigation-gated (Free plan): a page load without a session starts sign-in
// (or, when it cannot, answers the plan's sign-in page) and never reaches the carrier; a signed-in page load and every
// non-page request reach it. A host the server did not mark (Pro/Team) is fully public.
func TestFreeNavigationGateOnAPublicTunnelRoute(t *testing.T) {
	match, decision := browserTestMatch()
	host := match.Host
	rule := match.Rule
	rule.AccessMode = "public"
	rule.MatchType, rule.Hostname, rule.PathPrefix, rule.Target = route.MatchExact, host, "/", rule.ID
	rule.Protocol, rule.OriginScheme, rule.HostID, rule.ObservedState = "http", "http", decision.Binding.HostID, "ready"
	rule.ConfigContentHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	rule.EdgeFailureDomain, rule.AssignmentID, rule.AssignmentGeneration = "region_01", "assignment_01", 1
	paid := rule
	paid.ID, paid.RouteID, paid.Hostname, paid.Target = "route_paid", "route_paid", "paid.customer.test", "route_paid"
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	if err := routes.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule, paid}, func(context.Context, []route.RouteRule) error { return nil }, 0); err != nil {
		t.Fatal(err)
	}
	authority := &browserAuthorityTest{decision: decision}
	var calls int
	canonical := durablePolicyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	policy, err := NewGatewayWithTransports(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 8192, MaxBodyBytes: 4096, Routes: routes,
		BrowserAccess:   &BrowserAccess{Authority: authority},
		NavigationGated: func(h string) bool { return h == host }},
		"127.0.0.1:1", durablePolicyRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.EOF }), canonical)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(target, method string, headers map[string]string, cookie bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "https://"+target+"/page", strings.NewReader(""))
		request.Host, request.RemoteAddr = target, "198.51.100.7:4000"
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		if cookie {
			request.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "session"})
		}
		recorder := httptest.NewRecorder()
		policy.ServeHTTP(recorder, request)
		return recorder
	}
	navigate := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "none"}
	// page loads without a session never reach the app
	if got := serve(host, "GET", navigate, false); got.Code != http.StatusSeeOther || !strings.HasPrefix(got.Header().Get("Location"), testLoginOrigin) || calls != 0 {
		t.Fatalf("document navigation: %d %q calls %d", got.Code, got.Header().Get("Location"), calls)
	}
	for name, headers := range map[string]map[string]string{
		"no Sec-Fetch, Accept html": {"Accept": "text/html"},
		"iframe":                    {"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"},
	} {
		if got := serve(host, "GET", headers, false); got.Code != http.StatusUnauthorized || !strings.Contains(got.Body.String(), "free plan") || calls != 0 {
			t.Fatalf("%s: %d calls %d body %q", name, got.Code, calls, got.Body.String())
		}
	}
	// everything that is not a page load passes unauthenticated
	for name, c := range map[string]struct {
		method  string
		headers map[string]string
	}{
		"fetch":        {"GET", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}},
		"subresource":  {"GET", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}},
		"webhook post": {"POST", map[string]string{"Content-Type": "application/json"}},
		"api client":   {"GET", map[string]string{"Accept": "application/json"}},
	} {
		before := calls
		if got := serve(host, c.method, c.headers, false); got.Code != http.StatusOK || calls != before+1 {
			t.Fatalf("%s: %d calls %d", name, got.Code, calls-before)
		}
	}
	// a signed-in page load reaches the app
	before := calls
	if got := serve(host, "GET", navigate, true); got.Code != http.StatusOK || calls != before+1 {
		t.Fatalf("signed-in navigation: %d calls %d body %q", got.Code, calls-before, got.Body.String())
	}
	// a revoked/denied session is not let through
	authority.revoked.Store(true)
	before = calls
	if got := serve(host, "GET", navigate, true); got.Code == http.StatusOK || calls != before {
		t.Fatalf("denied session: %d calls %d", got.Code, calls-before)
	}
	// a host the server did not mark (Pro, Team, operator override) stays fully public
	before = calls
	if got := serve("paid.customer.test", "GET", navigate, false); got.Code != http.StatusOK || calls != before+1 {
		t.Fatalf("unmarked host navigation: %d calls %d", got.Code, calls-before)
	}
}
