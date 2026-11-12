package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

type browserAuthorityTest struct {
	revoked  atomic.Bool
	calls    atomic.Int32
	decision connectorprotocol.IngressDecision
	failure  error
}

func (a *browserAuthorityTest) Begin(context.Context, string, string) (control.BrowserBegin, error) {
	a.calls.Add(1)
	return control.BrowserBegin{TransactionID: "bat_test", State: "nonce", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (a *browserAuthorityTest) Redeem(_ context.Context, tx, state, handoff, host string) (control.BrowserSession, error) {
	a.calls.Add(1)
	if handoff == "bah_denied" {
		return control.BrowserSession{}, control.ErrBrowserDenied
	}
	if tx != "bat_test" || state != "nonce" || handoff != "bah_test" || host != "app.example.test" {
		return control.BrowserSession{}, control.ErrBrowserDenied
	}
	return control.BrowserSession{Token: "session", ReturnPath: "/app?q=1", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (a *browserAuthorityTest) Authorize(context.Context, control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error) {
	a.calls.Add(1)
	if a.revoked.Load() {
		return connectorprotocol.IngressDecision{}, control.ErrBrowserDenied
	}
	next := a.decision
	next.IssuedAt = time.Now().UTC()
	next.ExpiresAt = next.IssuedAt.Add(150 * time.Millisecond)
	next.VisitorAssertion = "visitor-jwt-" + next.IssuedAt.Format(time.RFC3339Nano) // fresh per answer, never a revocation
	return next, a.failure
}

func TestBrowserCallbackRejectsForgedReturnBeforeRedemption(t *testing.T) {
	for _, tc := range []struct {
		name, origin, cookie, body string
		status, calls              int
	}{
		{"valid", "https://login.example.test", "__Host-pb-handoff=bat_test.nonce", "transaction_id=bat_test&handoff=bah_test", 303, 1},
		{"sibling", "https://evil.example.test", "__Host-pb-handoff=bat_test.nonce", "transaction_id=bat_test&handoff=bah_test", 403, 0},
		{"duplicate cookie", "https://login.example.test", "__Host-pb-handoff=bat_test.nonce; __Host-pb-handoff=bat_test.evil", "transaction_id=bat_test&handoff=bah_test", 403, 0},
		{"wrong transaction", "https://login.example.test", "__Host-pb-handoff=bat_other.nonce", "transaction_id=bat_test&handoff=bah_test", 403, 0},
		{"duplicate field", "https://login.example.test", "__Host-pb-handoff=bat_test.nonce", "transaction_id=bat_test&handoff=bah_test&handoff=evil", 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &browserAuthorityTest{}
			b := BrowserAccess{Authority: a, LoginOrigin: "https://login.example.test"}
			r := httptest.NewRequest("POST", "https://app.example.test"+browserCallbackPath, strings.NewReader(tc.body))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Cookie", tc.cookie)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			b.callback(w, r)
			if w.Code != tc.status || int(a.calls.Load()) != tc.calls {
				t.Fatalf("status=%d calls=%d", w.Code, a.calls.Load())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("callback cacheable")
			}
			if tc.status == 303 {
				if w.Header().Get("Location") != "/app?q=1" {
					t.Fatal("return path changed")
				}
				for _, c := range w.Result().Cookies() {
					if c.Name == browserEdgeCookie && (!c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" || c.SameSite != http.SameSiteLaxMode) {
						t.Fatal("unsafe edge cookie")
					}
				}
			}
		})
	}
}

func TestBrowserNavigationAndWebhookSemantics(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "DELETE", "OPTIONS", "GET"} {
		a := &browserAuthorityTest{}
		b := BrowserAccess{Authority: a, LoginOrigin: "https://login.example.test"}
		r := httptest.NewRequest(method, "https://app.example.test/hook", nil)
		w := httptest.NewRecorder()
		b.begin(w, r, "app.example.test")
		if w.Code != 401 || a.calls.Load() != 0 || w.Header().Get("Location") != "" {
			t.Fatalf("%s redirected non-navigation", method)
		}
	}
	a := &browserAuthorityTest{}
	b := BrowserAccess{Authority: a, LoginOrigin: "https://login.example.test"}
	r := httptest.NewRequest("GET", "https://app.example.test/app?q=1", nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	b.begin(w, r, "app.example.test")
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil || w.Code != 303 || u.Host != "login.example.test" || u.Query().Get("transaction_id") != "bat_test" || len(u.Query()) != 1 {
		t.Fatal("untrusted login redirect")
	}
	if strings.Contains(w.Header().Get("Location"), "nonce") {
		t.Fatal("state in URL")
	}
}
func TestBrowserReturnAndApplicationCredentialIsolation(t *testing.T) {
	for _, p := range []string{"//evil.test/x", "/%2fexample.test", "/%5cevil.test", "/a\r\nb", "https://evil.test", "/" + strings.Repeat("x", 2048)} {
		if safeBrowserReturn(p) {
			t.Fatalf("unsafe return accepted: %q", p)
		}
	}
	h := http.Header{"Cookie": []string{"app_session=keep; __Host-pb-edge=secret; __Host-pb-session=secret; pb-dev-session=secret"}, "Authorization": []string{"Bearer application"}, "Paperboat-Token": []string{"secret"}}
	stripBrowserRequestCredentials(h)
	if h.Get("Cookie") != "app_session=keep" || h.Get("Authorization") != "Bearer application" || h.Get("Paperboat-Token") != "" {
		t.Fatal("application/auth credential separation failed")
	}
	h = http.Header{"Set-Cookie": []string{"__Host-pb-edge=forged; Secure; Path=/", "app=keep; Path=/"}}
	stripBrowserResponseCredentials(h)
	if len(h.Values("Set-Cookie")) != 1 || h.Get("Set-Cookie") != "app=keep; Path=/" {
		t.Fatal("origin set access cookie")
	}
}
func browserTestMatch() (route.RouteMatch, connectorprotocol.IngressDecision) {
	d := publicTCPDecision(time.Now().UTC(), 4001)
	d.Binding.Protocol = "http"
	d.Binding.OriginScheme = "http"
	d.Binding.PathPrefix = "/"
	d.Binding.ListenerID = ""
	d.Binding.PublicPort = 0
	d.Binding.Audience = "private"
	d.PrincipalID = "viewer"
	d.GrantID = "grant"
	d.GrantGeneration = 1
	d.IssuedAt = time.Now().UTC()
	d.ExpiresAt = d.IssuedAt.Add(150 * time.Millisecond)
	b := d.Binding
	m := route.RouteMatch{Host: b.Hostname, Rule: route.RouteRule{ID: b.RouteID, RouteID: b.RouteID, Revision: b.RouteGeneration, RouteGeneration: b.RouteGeneration, AccountID: b.AccountID, TunnelID: b.TunnelID, AccessMode: b.Audience, Node: d.EdgeNodeID, EdgeProcessEpoch: d.EdgeProcessEpoch, ConnectorID: d.ConnectorID, ConnectorSessionID: d.SessionID, ConnectorProcessGeneration: d.ProcessGeneration, ConfigGeneration: d.ConfigGeneration, Kind: route.TunnelHTTPSWSS}}
	return m, d
}

func TestBrowserDecisionRequiresExactViewerPolicyGeneration(t *testing.T) {
	match, decision := browserTestMatch()
	match.Rule.ViewerPolicyGeneration = decision.PolicyGeneration
	if !browserDecisionMatches(decision, match) {
		t.Fatal("matching viewer policy generation was rejected")
	}
	decision.PolicyGeneration++
	if browserDecisionMatches(decision, match) {
		t.Fatal("stale viewer policy generation was accepted")
	}
}

func TestBrowserDecisionOnANewerRouteGenerationServesTheActiveRule(t *testing.T) {
	match, decision := browserTestMatch()
	decision.Binding.RouteGeneration++
	if !browserDecisionMatches(decision, match) {
		t.Fatal("decision on the newer route generation was refused while the route update is applied")
	}
	match.Rule.RouteGeneration += 2
	if browserDecisionMatches(decision, match) {
		t.Fatal("decision older than the rule was accepted")
	}
}

func TestBrowserAuthorizationOutlivesTTLUntilRevokedAndDeniesNewAdmissions(t *testing.T) {
	m, d := browserTestMatch()
	a := &browserAuthorityTest{decision: d}
	b := BrowserAccess{Authority: a, LoginOrigin: "https://login.example.test"}
	r := httptest.NewRequest("GET", "https://"+m.Host+"/", nil)
	r.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "token"})
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	allowed, finish, ok := b.authorize(w, r, m)
	if !ok {
		t.Fatalf("authorize status=%d", w.Code)
	}
	select {
	case <-allowed.Context().Done():
		t.Fatal("admitted browser request expired")
	case <-time.After(300 * time.Millisecond):
	}
	a.revoked.Store(true)
	select {
	case <-allowed.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("revocation retained browser request")
	}
	finish()
	a.revoked.Store(false)
	a.failure = control.ErrBrowserDenied
	w = httptest.NewRecorder()
	_, finish, ok = b.authorize(w, r, m)
	finish()
	if ok || w.Code != 403 {
		t.Fatal("denial forwarded")
	}
	a.failure = errors.New("control lost")
	w = httptest.NewRecorder()
	_, finish, ok = b.authorize(w, r, m)
	finish()
	if ok || w.Code != 503 {
		t.Fatal("control loss forwarded")
	}
}

func TestBrowserCookieRequestsFromSiblingHostsAreRefused(t *testing.T) {
	// Without a public-suffix listing a sibling preview host is same-site, so
	// the edge must refuse cookie-carrying requests it started other than as a
	// top-level document navigation.
	m, d := browserTestMatch()
	for _, c := range []struct {
		name, site, mode, dest, method string
		origin, referer                string
		allowed                        bool
	}{
		{"no fetch metadata", "", "", "", "GET", "", "", false},
		{"no fetch metadata, same-origin Origin", "", "", "", "GET", "https://" + m.Host, "", true},
		{"no fetch metadata, sibling Origin", "", "", "", "GET", "https://other.example.test", "", false},
		{"no fetch metadata, null Origin", "", "", "", "GET", "null", "", false},
		{"no fetch metadata, same-origin Referer", "", "", "", "GET", "", "https://" + m.Host + "/page", true},
		{"no fetch metadata, sibling Referer", "", "", "", "GET", "", "https://other.example.test/page", false},
		{"no fetch metadata, http Referer", "", "", "", "GET", "", "http://" + m.Host + "/page", false},
		{"unknown fetch site, no Origin", "bogus", "cors", "empty", "GET", "", "", false},
		{"same origin fetch", "same-origin", "cors", "empty", "GET", "", "", true},
		{"typed address", "none", "navigate", "document", "GET", "", "", true},
		{"sibling top-level navigation", "same-site", "navigate", "document", "GET", "", "", true},
		{"cross-site top-level navigation", "cross-site", "navigate", "document", "GET", "", "", true},
		{"sibling fetch", "same-site", "cors", "empty", "GET", "", "", false},
		{"sibling image", "same-site", "no-cors", "image", "GET", "", "", false},
		{"sibling websocket", "same-site", "websocket", "websocket", "GET", "", "", false},
		{"sibling iframe", "same-site", "navigate", "iframe", "GET", "", "", false},
		{"sibling form post", "same-site", "navigate", "document", "POST", "", "", false},
		{"cross-site fetch", "cross-site", "cors", "empty", "GET", "", "", false},
	} {
		a := &browserAuthorityTest{decision: d}
		b := BrowserAccess{Authority: a, LoginOrigin: "https://login.example.test"}
		r := httptest.NewRequest(c.method, "https://"+m.Host+"/", nil)
		r.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "token"})
		for k, v := range map[string]string{"Sec-Fetch-Site": c.site, "Sec-Fetch-Mode": c.mode, "Sec-Fetch-Dest": c.dest} {
			if v != "" {
				r.Header.Set(k, v)
			}
		}
		if c.method == "POST" {
			r.Header.Set("Origin", "https://"+m.Host)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.referer != "" {
			r.Header.Set("Referer", c.referer)
		}
		w := httptest.NewRecorder()
		_, finish, ok := b.authorize(w, r, m)
		finish()
		if ok != c.allowed || (!c.allowed && (w.Code != 403 || a.calls.Load() != 0)) {
			t.Fatalf("%s: allowed=%v status=%d", c.name, ok, w.Code)
		}
	}
}

func TestBrowserCallbackDeniedAnswers403WithSwitchAccountLink(t *testing.T) {
	a := &browserAuthorityTest{}
	b := BrowserAccess{Authority: a, LoginOrigin: "https://login.example.test"}
	r := httptest.NewRequest("POST", "https://app.example.test"+browserCallbackPath, strings.NewReader("transaction_id=bat_test&handoff=bah_denied"))
	r.Header.Set("Origin", "https://login.example.test")
	r.Header.Set("Cookie", "__Host-pb-handoff=bat_test.nonce")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	b.callback(w, r)
	body := w.Body.String()
	if w.Code != 403 || !strings.Contains(body, deniedMessage) || !strings.Contains(body, "Use a different account") ||
		!strings.Contains(body, "https://login.example.test/.paperboat/access/login?switch=1&amp;transaction_id=bat_test") {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
}

func TestBrowserUnauthenticatedBeginStillRedirectsToSignIn(t *testing.T) {
	b := BrowserAccess{Authority: &browserAuthorityTest{}, LoginOrigin: "https://login.example.test"}
	r := httptest.NewRequest("GET", "https://app.example.test/x", nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	b.begin(w, r, "app.example.test")
	if w.Code != 303 || w.Header().Get("Location") != "https://login.example.test/.paperboat/access/login?transaction_id=bat_test" {
		t.Fatalf("status=%d location=%s", w.Code, w.Header().Get("Location"))
	}
}

// A gated route's app receives the server-signed visitor assertion; a visitor's own copy never survives,
// and a request without a browser decision (public route) carries none.
func TestVisitorAssertionReachesOnlyGatedApps(t *testing.T) {
	_, d := browserTestMatch()
	d.VisitorAssertion = "signed.visitor.jwt"
	gated := httptest.NewRequest("GET", "https://app.example.test/", nil)
	gated.Header.Set("Paperboat-Visitor-Assertion", "forged")
	stripPrivate(gated.Header, string(route.TunnelHTTPSWSS))
	gated = gated.WithContext(context.WithValue(gated.Context(), browserDecisionKey{}, d))
	setVisitorAssertion(gated)
	if values := gated.Header.Values("Paperboat-Visitor-Assertion"); len(values) != 1 || values[0] != "signed.visitor.jwt" {
		t.Fatalf("gated assertion = %q", values)
	}
	public := httptest.NewRequest("GET", "https://app.example.test/", nil)
	public.Header.Set("Paperboat-Visitor-Assertion", "forged")
	stripPrivate(public.Header, string(route.TunnelHTTPSWSS))
	setVisitorAssertion(public)
	if public.Header.Get("Paperboat-Visitor-Assertion") != "" {
		t.Fatal("a visitor's own assertion reached a public app")
	}
}
