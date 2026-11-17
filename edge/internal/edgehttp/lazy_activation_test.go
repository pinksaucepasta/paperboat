package edgehttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type lazyBodyProbe struct{ reads atomic.Int32 }

func (b *lazyBodyProbe) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (*lazyBodyProbe) Close() error               { return nil }

type lazyAuthorityFixture struct {
	browserAuthorityTest
	activate func(context.Context, control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error)
	calls    atomic.Int32
}

func (a *lazyAuthorityFixture) Activate(ctx context.Context, in control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error) {
	a.calls.Add(1)
	return a.activate(ctx, in)
}

func lazyRuleAndDecision(host string) (route.RouteRule, connectorprotocol.IngressDecision) {
	_, decision := browserTestMatch()
	decision.Binding.Hostname = host
	decision.Binding.Audience = "private"
	decision.IssuedAt = time.Now().UTC()
	decision.ExpiresAt = decision.IssuedAt.Add(time.Second)
	rule := route.RouteRule{
		ID: decision.Binding.RouteID, RouteID: decision.Binding.RouteID, Revision: decision.Binding.RouteGeneration, RouteGeneration: decision.Binding.RouteGeneration,
		AccountID: decision.Binding.AccountID, TunnelID: decision.Binding.TunnelID, AccessMode: "private", ResourceKind: "tunnel",
		Node: decision.EdgeNodeID, EdgeProcessEpoch: decision.EdgeProcessEpoch, ConnectorID: decision.ConnectorID, ConnectorSessionID: decision.SessionID,
		ConnectorProcessGeneration: decision.ProcessGeneration, ConfigGeneration: decision.ConfigGeneration, Kind: route.TunnelHTTPSWSS,
		MatchType: route.MatchExact, Hostname: host, PathPrefix: "/", Target: "route_binding_lazy", Protocol: "http", ObservedState: "ready",
	}
	return rule, decision
}

func newLazyPolicy(t *testing.T, registry *route.GenerationRegistry, authority *lazyAuthorityFixture, next http.Handler) *Policy {
	t.Helper()
	p, err := New(Config{BrowserAccess: &BrowserAccess{Authority: authority, LazyAuthority: authority, LoginOrigin: "https://login.example.test"}, PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: registry, RequestID: func() string { return "request_lazy" }}, next)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLazyActivationBeforeBodyReadAndSingleAcquireRetry(t *testing.T) {
	host := "p3000-abcdefghijklmnop.preview.example.test"
	rule, decision := lazyRuleAndDecision(host)
	registry := route.NewGenerationRegistry(4)
	body := &lazyBodyProbe{}
	authority := &lazyAuthorityFixture{}
	authority.decision = decision
	authority.activate = func(_ context.Context, input control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error) {
		if body.reads.Load() != 0 {
			t.Fatal("application body read before activation")
		}
		if input.Host != host || input.Token != "browser-token" || input.ResourceKind != "lazy_policy" || input.ResourceID != "" || input.RouteID != "" {
			t.Fatalf("activation input=%#v", input)
		}
		if err := registry.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, time.Second); err != nil {
			t.Fatal(err)
		}
		return decision, nil
	}
	forwarded := atomic.Bool{}
	p := newLazyPolicy(t, registry, authority, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodPost, "https://"+host+"/submit", body)
	r.Host = host
	r.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "browser-token"})
	r.Header.Set("Origin", "https://"+host)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || !forwarded.Load() || authority.calls.Load() != 1 || body.reads.Load() != 0 {
		t.Fatalf("status=%d forwarded=%v activations=%d body_reads=%d", w.Code, forwarded.Load(), authority.calls.Load(), body.reads.Load())
	}
}

func TestLazyActivationRequiresCredentialAndCanonicalHostname(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		credential bool
	}{
		{"anonymous", "p3000-abcdefghijklmnop.preview.example.test", false},
		{"ordinary preview", "ordinary.preview.example.test", true},
		{"noncanonical host", "P3000-abcdefghijklmnop.preview.example.test", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := &lazyAuthorityFixture{activate: func(context.Context, control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error) {
				t.Fatal("unexpected activation")
				return connectorprotocol.IngressDecision{}, nil
			}}
			p := newLazyPolicy(t, route.NewGenerationRegistry(1), authority, http.NotFoundHandler())
			r := httptest.NewRequest(http.MethodPost, "https://"+tc.host+"/", nil)
			r.Host = tc.host
			if tc.credential {
				r.Header.Set("Paperboat-Access-Token", "machine-token")
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if authority.calls.Load() != 0 {
				t.Fatalf("activations=%d", authority.calls.Load())
			}
		})
	}
}

func TestLazyActivationCancellationLeavesBodyUnread(t *testing.T) {
	host := "p3000-abcdefghijklmnop.preview.example.test"
	body := &lazyBodyProbe{}
	authority := &lazyAuthorityFixture{activate: func(ctx context.Context, _ control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error) {
		<-ctx.Done()
		return connectorprotocol.IngressDecision{}, ctx.Err()
	}}
	p := newLazyPolicy(t, route.NewGenerationRegistry(1), authority, http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "https://"+host+"/", body).WithContext(ctx)
	r.Host = host
	r.Header.Set("Paperboat-Access-Token", "machine-token")
	done := make(chan struct{})
	go func() { p.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	for authority.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("activation ignored cancellation")
	}
	if body.reads.Load() != 0 {
		t.Fatal("application body read while activation canceled")
	}
}

func TestLazyActivationErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		retry  string
	}{
		{"offline", control.ErrLazyHostOffline, 503, ""},
		{"origin", control.ErrLazyOriginUnavailable, 503, ""},
		{"timeout", control.ErrLazyActivationTimeout, 504, ""},
		{"generation", control.ErrLazyGenerationConflict, 409, ""},
		{"capacity", control.ErrLazyCapacity, 429, "1"},
		{"forwarding", control.ErrLazyForwardingFailed, 503, ""},
		{"cooldown", control.ErrLazyCooldown, 503, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://preview.example.test/", nil)
			w := httptest.NewRecorder()
			lazyActivationError(w, r, tc.err)
			if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retry {
				t.Fatalf("status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
			}
		})
	}
}

var (
	refMask  = regexp.MustCompile(`support_[0-9a-f-]{36}`)
	timeMask = regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC`)
	// The handoff cookie's Expires moves with the clock, so two otherwise equal answers can straddle a second.
	expiresMask = regexp.MustCompile(`Expires=[^;\]]*`)
)

// Anyone can sign up, so a signed-in visitor must not tell a name that does not exist from a private app they may not
// open: same status, headers and body (reference and time aside), whether the name is a lazy preview or any other host.
func TestSignedInUnknownHostLooksLikeDeniedPrivateApp(t *testing.T) {
	const denied = "secret.custom.example.test"
	const lazy = "p3000-abcdefghijklmnop.preview.example.test"
	rule, decision := lazyRuleAndDecision(denied)
	registry := route.NewGenerationRegistry(4)
	if err := registry.ApplyGeneration(context.Background(), 1, []route.RouteRule{rule}, func(context.Context, []route.RouteRule) error { return nil }, time.Second); err != nil {
		t.Fatal(err)
	}
	controlErr := control.ErrBrowserDenied
	authority := &lazyAuthorityFixture{activate: func(context.Context, control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error) {
		return connectorprotocol.IngressDecision{}, controlErr
	}}
	authority.decision, authority.failure = decision, control.ErrBrowserDenied
	p := newLazyPolicy(t, registry, authority, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("reached the app") }))
	get := func(host string, signedIn bool, edit ...func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		r.Host = host
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Site", "none")
		if signedIn {
			r.AddCookie(&http.Cookie{Name: browserEdgeCookie, Value: "browser-token"})
		}
		for _, e := range edit {
			e(r)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	shape := func(w *httptest.ResponseRecorder) string {
		h := w.Header().Clone()
		h.Del(referenceHeader)
		return fmt.Sprint(w.Code, expiresMask.ReplaceAllString(fmt.Sprint(h), "Expires=X"), timeMask.ReplaceAllString(refMask.ReplaceAllString(w.Body.String(), "REF"), "TIME"))
	}
	want := shape(get(denied, true))
	if w := get(denied, true); w.Code != 404 || !strings.Contains(w.Body.String(), "Use a different account") || !strings.Contains(w.Body.String(), notFoundMessage) {
		t.Fatalf("denied private app: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, host := range []string{lazy, "nothing.custom.example.test"} {
		if got := shape(get(host, true)); got != want {
			t.Fatalf("%s differs from a denied private app:\n%s\n%s", host, got, want)
		}
	}
	// A request the edge refuses before asking control (another site's subresource) and a machine token's page load are
	// answered alike for every name too.
	// Malformed credentials, a session control does not know, and an anonymous request that is no page load: the same
	// for every name.
	machine := func(r *http.Request) { r.Header.Set("Paperboat-Access-Token", "machine-token") }
	dupCookie := func(r *http.Request) {
		r.Header.Add("Cookie", "__Host-pb-x=1; __Host-pb-x=2")
	}
	badToken := func(r *http.Request) { r.Header.Set("Paperboat-Access-Token", " padded") }
	noPage := func(r *http.Request) { r.Header.Del("Sec-Fetch-Mode"); r.Header.Del("Sec-Fetch-Dest") }
	for name, tc := range map[string]struct {
		signedIn bool
		edit     func(*http.Request)
		err      error
	}{
		"duplicate cookie":   {true, dupCookie, control.ErrBrowserDenied},
		"malformed token":    {true, badToken, control.ErrBrowserDenied},
		"anonymous no page":  {false, noPage, control.ErrBrowserDenied},
		"unknown session":    {true, nil, control.ErrBrowserUnauthenticated},
		"unknown machine id": {true, machine, control.ErrBrowserUnauthenticated},
	} {
		authority.failure, controlErr = tc.err, tc.err
		edit := tc.edit
		if edit == nil {
			edit = func(*http.Request) {}
		}
		want := shape(get(denied, tc.signedIn, edit))
		for _, host := range []string{lazy, "nothing.custom.example.test"} {
			if got := shape(get(host, tc.signedIn, edit)); got != want {
				t.Fatalf("%s, %s differs from a private app:\n%s\n%s", name, host, got, want)
			}
		}
	}
	authority.failure, controlErr = control.ErrBrowserDenied, control.ErrBrowserDenied
	crossSite := func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "cors")
	}
	for name, edit := range map[string]func(*http.Request){"cross-site": crossSite, "machine token": machine} {
		want := shape(get(denied, true, edit))
		for _, host := range []string{lazy, "nothing.custom.example.test"} {
			if got := shape(get(host, true, edit)); got != want {
				t.Fatalf("%s, %s differs from a denied private app:\n%s\n%s", name, host, got, want)
			}
		}
	}
}

// Should control refuse Begin (the server answers it for every well-formed name now, as a denied sign-in), the edge
// answers a signed-out browser with the 404 page, never the 403 "no access" page this used to be.
type beginDeniedAuthority struct{ lazyAuthorityFixture }

func (*beginDeniedAuthority) Begin(context.Context, string, string) (control.BrowserBegin, error) {
	return control.BrowserBegin{}, control.ErrBrowserDenied
}

func TestSignedOutUnknownHostNavigationIs404(t *testing.T) {
	authority := &beginDeniedAuthority{}
	p, err := New(Config{BrowserAccess: &BrowserAccess{Authority: authority, LoginOrigin: "https://login.example.test"}, PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: route.NewGenerationRegistry(1)}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "https://hello.custom.example.test/", nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 404 || !strings.Contains(w.Body.String(), notFoundMessage) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
