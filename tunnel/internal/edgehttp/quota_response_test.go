package edgehttp

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// F13-03: a stream refused because the workspace is out of allowance gets the typed answer a
// visitor and a script can act on; every other upstream failure stays a plain 502.
func TestProxyAnswersQuotaRefusalWithTypedTooManyRequests(t *testing.T) {
	serve := func(err error) *httptest.ResponseRecorder {
		proxy := &httputil.ReverseProxy{Director: func(r *http.Request) { r.URL = &url.URL{Scheme: "http", Host: "origin.invalid"} }, Transport: failingTransport{err}}
		configureProxyDiagnostics(proxy, nil)
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://tunnel.example.test/", nil))
		return recorder
	}
	quota := serve(fmt.Errorf("open stream: %w", ErrIngressQuotaExhausted))
	if quota.Code != http.StatusTooManyRequests || quota.Header().Get("X-Paperboat-Error") != "bandwidth_quota_exhausted" || !strings.Contains(quota.Body.String(), "bandwidth allowance") {
		t.Fatalf("quota refusal: %d %q %q", quota.Code, quota.Header().Get("X-Paperboat-Error"), quota.Body.String())
	}
	other := serve(errors.New("connection refused"))
	if other.Code != http.StatusBadGateway || other.Header().Get("X-Paperboat-Error") != "" {
		t.Fatalf("ordinary upstream failure: %d %q", other.Code, other.Header().Get("X-Paperboat-Error"))
	}
}
