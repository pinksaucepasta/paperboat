package edgehttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
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
	busy := serve(errors.Join(ErrDataCarrierRouteTransport, datacarrier.ErrStreamLimit))
	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("X-Paperboat-Error") != "carrier_stream_limit" || busy.Header().Get("Retry-After") != "2" {
		t.Fatalf("full carrier: %d %q %q", busy.Code, busy.Header().Get("X-Paperboat-Error"), busy.Header().Get("Retry-After"))
	}
	for legal, status := range map[bool]int{true: http.StatusUnavailableForLegalReasons, false: http.StatusForbidden} {
		blocked := serve(errors.Join(ErrDataCarrierRouteTransport, connectorprotocol.PublicationBlockedError{Legal: legal}))
		if blocked.Code != status || blocked.Header().Get("X-Paperboat-Error") != "publication_blocked" || !strings.Contains(blocked.Body.String(), "acceptable use policy") {
			t.Fatalf("takedown legal=%v: %d %q %q", legal, blocked.Code, blocked.Header().Get("X-Paperboat-Error"), blocked.Body.String())
		}
	}
	other := serve(errors.New("connection refused"))
	if other.Code != http.StatusBadGateway || other.Header().Get("X-Paperboat-Error") != "upstream_unavailable" || strings.Contains(other.Body.String(), "connection refused") || !strings.Contains(other.Body.String(), "could not reach the app") {
		t.Fatalf("ordinary upstream failure: %d %q %q", other.Code, other.Header().Get("X-Paperboat-Error"), other.Body.String())
	}
}

// Edge abuse plan 3.8 and 6.4: a refusal carries Retry-After and the typed code; a browser gets the
// branded page that reloads itself, an API client RFC 9457 problem JSON; the quota answer waits until
// the allowance resets and prints the date.
func TestRefusalNegotiatesPageOrProblemAndQuotaResetDate(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	resets := now.Add(36 * time.Hour)
	api := httptest.NewRecorder()
	writeQuotaRefusal(api, httptest.NewRequest(http.MethodGet, "https://app.example.test/v1/x", nil), resets, now)
	var body problem
	if err := json.Unmarshal(api.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem json: %v %q", err, api.Body.String())
	}
	if api.Code != http.StatusTooManyRequests || api.Header().Get("Content-Type") != "application/problem+json" || api.Header().Get("Retry-After") != "129600" ||
		body.Type != "urn:paperboat:error:bandwidth_quota_exhausted" || body.Status != 429 || body.RetryAfter != 129600 || body.Reference == "" || !strings.Contains(body.Detail, "13 Nov 2026 00:00 UTC") {
		t.Fatalf("api refusal: %d %v %+v", api.Code, api.Header(), body)
	}
	browser := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://app.example.test/", nil)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	writeRefusal(browser, request, http.StatusTooManyRequests, "edge_rate_limited", busyTitle, busyMessage, 1500*time.Millisecond)
	page := browser.Body.String()
	if browser.Code != 429 || browser.Header().Get("Retry-After") != "2" || !strings.HasPrefix(browser.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(page, `<meta http-equiv="refresh" content="2">`) || !strings.Contains(page, busyMessage) || strings.Contains(page, "edge_rate_limited") {
		t.Fatalf("browser refusal: %d %v\n%s", browser.Code, browser.Header(), page)
	}
	unknown := httptest.NewRecorder()
	writeQuotaRefusal(unknown, request, time.Time{}, now)
	if unknown.Header().Get("Retry-After") != "" || strings.Contains(unknown.Body.String(), "http-equiv") {
		t.Fatalf("quota without a reset time: %v", unknown.Header())
	}
}

// Public Suffix List prerequisite (edge abuse plan 3.6.1): every cookie the edge sets is host-only
// (__Host- forbids a Domain attribute), so listing the preview and tunnel base domains on the PSL
// changes nothing for Paperboat and isolates each name for browsers and blocklists.
func TestEdgeCookiesAreHostOnly(t *testing.T) {
	for _, name := range []string{browserEdgeCookie, browserHandoffCookie} {
		if !strings.HasPrefix(name, "__Host-") {
			t.Fatalf("edge cookie %q is not host-only", name)
		}
	}
}
