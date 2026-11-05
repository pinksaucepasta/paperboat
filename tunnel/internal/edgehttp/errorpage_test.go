package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

func assertBrandedPage(t *testing.T, w *httptest.ResponseRecorder, status int, sentence string) {
	t.Helper()
	body := w.Body.String()
	if w.Code != status || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "paperboat") || !strings.Contains(body, sentence) {
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
