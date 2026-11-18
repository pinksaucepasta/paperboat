package edgehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// cancelingTransport ends the request's context the way end does, then fails like the carrier transport does once
// its stream lifetime (the request context) aborted the stream: "stream reset" joined with the context error.
type cancelingTransport struct{ end func() }

func (c cancelingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.end()
	return nil, errors.Join(ErrDataCarrierPreviewTransport, errors.New("stream reset"), r.Context().Err())
}

// Edge .32 roll on the test host, 2026-11-17 00:03:55Z: the server's terminal snapshot gave up on its /v1/runtime request
// while the old edge was handing over; the edge answered the departed client a 502 and logged edge_http_5xx, which
// the deploy gate counts as a user-visible failure. A client that left gets nothing and is no 5xx; a request the
// edge itself ended (its route lease, carrier or helper credential went away) still is one.
func TestProxyCountsNoFailureForAClientThatLeft(t *testing.T) {
	serve := func(end func(cancel context.CancelFunc, lease *route.StreamLease)) (*httptest.ResponseRecorder, int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		lease := route.NewStreamLease(ctx, route.RouteMatch{})
		defer lease.Close()
		observed := 0
		proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.Out.URL = &url.URL{Scheme: "http", Host: "origin.invalid"} },
			Transport: cancelingTransport{end: func() { end(cancel, lease) }}}
		configureProxyDiagnostics(proxy, func(context.Context, error) { observed++ })
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://preview.example.test/", nil).WithContext(lease.Context()))
		return recorder, observed
	}
	left, observed := serve(func(cancel context.CancelFunc, _ *route.StreamLease) { cancel() })
	if left.Body.Len() != 0 || left.Header().Get("X-Paperboat-Error") != "" || observed != 0 {
		t.Fatalf("client that left: status %d, %q, %d failures observed; want no answer and no failure", left.Code, left.Body.String(), observed)
	}
	ended, observed := serve(func(_ context.CancelFunc, lease *route.StreamLease) { _ = lease.Close() })
	if ended.Code != http.StatusBadGateway || ended.Header().Get("X-Paperboat-Error") != "upstream_unavailable" || observed != 1 {
		t.Fatalf("request the edge ended: status %d, %q, %d failures observed; want the 502 and one failure", ended.Code, ended.Header().Get("X-Paperboat-Error"), observed)
	}
}

// A full carrier, a takedown and any other upstream failure each get their typed answer; the rest stay a plain 502.
func TestProxyAnswersTypedRefusals(t *testing.T) {
	serve := func(err error) *httptest.ResponseRecorder {
		proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.Out.URL = &url.URL{Scheme: "http", Host: "origin.invalid"} }, Transport: failingTransport{err}}
		configureProxyDiagnostics(proxy, nil)
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://tunnel.example.test/", nil))
		return recorder
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
// branded page that reloads itself, an API client RFC 9457 problem JSON.
func TestRefusalNegotiatesPageOrProblem(t *testing.T) {
	api := httptest.NewRecorder()
	writeRefusal(api, httptest.NewRequest(http.MethodGet, "https://app.example.test/v1/x", nil), http.StatusTooManyRequests, "edge_rate_limited", busyTitle, busyMessage, 1500*time.Millisecond)
	var body problem
	if err := json.Unmarshal(api.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem json: %v %q", err, api.Body.String())
	}
	if api.Code != http.StatusTooManyRequests || api.Header().Get("Content-Type") != "application/problem+json" || api.Header().Get("Retry-After") != "2" ||
		body.Type != "urn:paperboat:error:edge_rate_limited" || body.Status != 429 || body.RetryAfter != 2 || body.Reference == "" {
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
