package edgehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
)

func assertBrandedPage(t *testing.T, w *httptest.ResponseRecorder, status int, sentence string) {
	t.Helper()
	body := w.Body.String()
	if w.Code != status || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "<!doctype html>") || !strings.Contains(body, `aria-label="Paperboat"`) || !strings.Contains(body, sentence) {
		t.Fatalf("status=%d type=%q want branded page %d containing %q: %q", w.Code, w.Header().Get("Content-Type"), status, sentence, body)
	}
}

type failingBeginAuthority struct{ browserAuthorityTest }

func (*failingBeginAuthority) Begin(context.Context, string, string) (control.BrowserBegin, error) {
	return control.BrowserBegin{}, errors.New("server answered 503")
}

func TestBrowserAuthorizationFailureRendersBrandedPage(t *testing.T) {
	b := BrowserAccess{Authority: &failingBeginAuthority{}, LoginOrigin: "https://login.example.test"}
	r := httptest.NewRequest("GET", "https://app.example.test/", nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	b.begin(w, r, "app.example.test")
	assertBrandedPage(t, w, 503, "could not check your access")
}

func TestEveryEdgeErrorStatusRendersBrandedPage(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 413, 429, 502, 503, 504} {
		w := httptest.NewRecorder()
		writeErrorPage(w, status, "")
		assertBrandedPage(t, w, status, defaultErrorMessages[status])
	}
	w := httptest.NewRecorder()
	writeErrorPage(w, 502, "<script>alert(1)</script>")
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("message not escaped")
	}
}

// No visitor-facing edge path may answer with Go's plain-text http.Error or http.NotFound.
func TestEdgeHTTPHasNoPlainTextErrors(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"http.Error(", "http.NotFound(", "http.NotFoundHandler("} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s uses %s; use writeErrorPage", f, banned)
			}
		}
	}
}

func TestUnavailableRouteHostServesBrandedBadGateway(t *testing.T) {
	routes := route.NewRegistry("preview.example.test", "runtime.example.test")
	unavailable := &route.UnavailableHosts{}
	unavailable.Replace([]string{"down.preview.example.test"})
	policy, err := New(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: routes, RouteUnavailable: unavailable.Has}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	for host, status := range map[string]int{"down.preview.example.test": 502, "unknown.preview.example.test": 404} {
		r := httptest.NewRequest("GET", "https://"+host+"/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		policy.ServeHTTP(w, r)
		sentence := notFoundMessage
		if status == 502 {
			sentence = "could not reach the app"
		}
		assertBrandedPage(t, w, status, sentence)
	}
}

func TestHoldForRouteWaitsOnlyWithinTheGrace(t *testing.T) {
	var calls atomic.Int32
	acquire := func(succeedAfter int32) func() (*route.StreamLease, route.RouteMatch, error) {
		return func() (*route.StreamLease, route.RouteMatch, error) {
			if calls.Add(1) > succeedAfter {
				return nil, route.RouteMatch{}, nil
			}
			return nil, route.RouteMatch{}, route.ErrNoMatch
		}
	}
	if _, _, err := holdForRoute(context.Background(), time.Second, acquire(3)); err != nil || calls.Load() != 4 {
		t.Fatalf("a route that appears within the grace: err=%v after %d calls", err, calls.Load())
	}
	calls.Store(0)
	if _, _, err := holdForRoute(context.Background(), 0, acquire(3)); !errors.Is(err, route.ErrNoMatch) || calls.Load() != 1 {
		t.Fatalf("without a grace the request is answered at once: err=%v after %d calls", err, calls.Load())
	}
	calls.Store(0)
	start := time.Now()
	if _, _, err := holdForRoute(context.Background(), 200*time.Millisecond, acquire(1<<30)); !errors.Is(err, route.ErrNoMatch) || time.Since(start) > time.Second {
		t.Fatalf("a route that never appears is refused after the grace: err=%v in %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls.Store(0)
	if _, _, err := holdForRoute(ctx, time.Minute, acquire(1<<30)); !errors.Is(err, route.ErrNoMatch) || calls.Load() != 1 {
		t.Fatalf("a request that ended stops waiting: err=%v after %d calls", err, calls.Load())
	}
}

// Each status shows its plain-language title, never the HTTP reason phrase; the reference id on the page, in the
// X-Paperboat-Reference header and in the edge log line are one value; the page ships its dark-mode styles.
func TestErrorPageTitleReferenceAndDarkMode(t *testing.T) {
	titles := map[int]string{
		400: "This link doesn't work", 401: "Sign in to continue", 403: "You don't have access to this app",
		404: "Page not found", 409: "Paperboat couldn't finish this request", 413: "Request too large",
		429: "Too many requests", 502: "This app isn't responding", 503: "Paperboat couldn't finish this request",
		504: "Paperboat couldn't finish this request",
	}
	actions := map[int]string{401: "Sign in", 409: "Try again", 429: "Try again", 502: "Try again", 503: "Try again", 504: "Try again"}
	reference := regexp.MustCompile(`^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	// Other tests share the package's 4xx sampler; this one needs every line.
	sampler := fourxxSample
	fourxxSample = &sampledLog{limit: 1 << 20}
	defer func() { fourxxSample = sampler }()
	for status, title := range titles {
		var logged bytes.Buffer
		restore := edgetelemetry.CaptureLog(&logged)
		w := httptest.NewRecorder()
		writeErrorPage(w, status, "")
		restore()
		body := w.Body.String()
		if !strings.Contains(body, "<h1>"+template.HTMLEscapeString(title)+"</h1>") {
			t.Errorf("%d: title %q missing: %q", status, title, body)
		}
		if strings.Contains(body, http.StatusText(status)) {
			t.Errorf("%d: the page shows the reason phrase %q", status, http.StatusText(status))
		}
		for _, want := range []string{"prefers-color-scheme:dark", `<meta name="color-scheme" content="light dark">`, `aria-label="Paperboat"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%d: page lacks %s", status, want)
			}
		}
		if got, want := strings.Contains(body, `class="action"`), actions[status] != ""; got != want || want && !strings.Contains(body, ">"+actions[status]+"</a>") {
			t.Errorf("%d: action %q present=%v", status, actions[status], got)
		}
		ref := w.Header().Get("X-Paperboat-Reference")
		if !reference.MatchString(ref) {
			t.Fatalf("%d: reference header %q", status, ref)
		}
		footer := "Error " + strconv.Itoa(status) + " · Reference <code class=\"ref\">" + ref + "</code> · "
		if !strings.Contains(body, footer) {
			t.Errorf("%d: footer %q missing: %q", status, footer, body)
		}
		var line struct {
			Operation string `json:"operation"`
			Reference string `json:"reference"`
			Status    int    `json:"status"`
		}
		wantEvent := "edge_http_4xx"
		if status >= 500 {
			wantEvent = "edge_http_5xx"
		}
		if err := json.Unmarshal(logged.Bytes(), &line); err != nil || line.Operation != wantEvent || line.Reference != ref || line.Status != status {
			t.Errorf("%d: logged %q (err %v), want %s with reference %s", status, logged.String(), err, wantEvent, ref)
		}
		if h := w.Header(); h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" || w.Code != status {
			t.Errorf("%d: headers %v code %d", status, h, w.Code)
		}
	}
}

func TestErrorPageReferenceIsPerRequest(t *testing.T) {
	a, b := httptest.NewRecorder(), httptest.NewRecorder()
	writeErrorPage(a, 502, "")
	writeErrorPage(b, 502, "")
	if a.Header().Get("X-Paperboat-Reference") == b.Header().Get("X-Paperboat-Reference") {
		t.Fatal("two failures share a reference")
	}
}

func TestErrorPageLinkIsTheAction(t *testing.T) {
	w := httptest.NewRecorder()
	writeErrorPageLink(w, 403, deniedMessage, "Use a different account", "https://login.example.test/x?a=1&b=2")
	body := w.Body.String()
	if !strings.Contains(body, `href="https://login.example.test/x?a=1&amp;b=2">Use a different account</a>`) {
		t.Fatalf("link action missing: %q", body)
	}
}

// A 4xx flood is sampled (20 lines a second, the next line says how many were dropped); 5xx are never sampled.
func TestEdgeHTTP4xxLogIsSampledAnd5xxIsNot(t *testing.T) {
	s := &sampledLog{limit: 3}
	start := time.Now()
	var logged int
	for i := 0; i < 10; i++ {
		if ok, _ := s.allow(start); ok {
			logged++
		}
	}
	if logged != 3 {
		t.Fatalf("%d lines logged in one second, want 3", logged)
	}
	if ok, suppressed := s.allow(start.Add(1100 * time.Millisecond)); !ok || suppressed != 7 {
		t.Fatalf("first line of the next second: ok=%v suppressed=%d, want true and 7", ok, suppressed)
	}
	var out bytes.Buffer
	restore := edgetelemetry.CaptureLog(&out)
	for i := 0; i < 3*fourxxLogPerSecond; i++ {
		writeErrorPage(httptest.NewRecorder(), 502, "")
	}
	restore()
	if got := strings.Count(out.String(), "\n"); got != 3*fourxxLogPerSecond {
		t.Fatalf("%d 5xx lines logged for %d 5xx pages", got, 3*fourxxLogPerSecond)
	}
}

var supportReference = regexp.MustCompile(`^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// A failing random source still gives a valid support_<uuid> reference and never a repeated one.
func TestNewReferenceFallsBackWhenRandomFails(t *testing.T) {
	previous := readRandom
	readRandom = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() { readRandom = previous }()
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		ref := newReference()
		if !supportReference.MatchString(ref) || seen[ref] {
			t.Fatalf("reference %q (repeat %v) from the fallback", ref, seen[ref])
		}
		seen[ref] = true
	}
}

// Every 5xx page and each sampled 4xx page hands its reference to the next heartbeat, which stores it in support_reports.
func TestErrorPageNotesItsReferenceForTheControlPlane(t *testing.T) {
	sampler := fourxxSample
	fourxxSample = &sampledLog{limit: 1 << 20}
	defer func() { fourxxSample = sampler }()
	edgetelemetry.TakeErrorPages()
	var out bytes.Buffer
	defer edgetelemetry.CaptureLog(&out)()
	want := map[string]int{}
	for _, status := range []int{404, 502, 503} {
		w := httptest.NewRecorder()
		writeErrorPage(w, status, "")
		want[w.Header().Get("X-Paperboat-Reference")] = status
	}
	got := map[string]int{}
	for _, page := range edgetelemetry.TakeErrorPages() {
		got[page.Reference] = page.Status
	}
	if len(got) != 3 {
		t.Fatalf("noted %v, want %v", got, want)
	}
	for reference, status := range want {
		if got[reference] != status {
			t.Errorf("reference %s noted with status %d, want %d", reference, got[reference], status)
		}
	}
	if again := edgetelemetry.TakeErrorPages(); len(again) != 0 {
		t.Fatalf("pages noted twice: %v", again)
	}
}
