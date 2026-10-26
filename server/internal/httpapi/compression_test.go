package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonBody(n int) string { return `{"items":"` + strings.Repeat("machine ", n/8) + `"}` }

func serveCompressed(t *testing.T, r *http.Request, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	compressResponses(handler).ServeHTTP(recorder, r)
	return recorder
}

func machineRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("X-Paperboat-Machine-Proof", "proof")
	return r
}

func jsonResponder(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}
}

// PR-03: a machine's large JSON response is gzipped and decodes back to the same body.
func TestCompressResponsesGzipsLargeJSONForMachines(t *testing.T) {
	body := jsonBody(250_000)
	recorder := serveCompressed(t, machineRequest(http.MethodGet, "/v1/machines"), jsonResponder(body))
	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if recorder.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Vary = %q", recorder.Header().Get("Vary"))
	}
	if recorder.Body.Len() >= len(body)/10 {
		t.Fatalf("compressed %d bytes to %d", len(body), recorder.Body.Len())
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || string(decoded) != body {
		t.Fatalf("decoded body differs (err %v)", err)
	}
}

// Everything that must stay identity: peer-network config (bearer relay grants, BREACH), cookie sessions,
// clients that did not ask, small bodies, non-JSON and empty responses.
func TestCompressResponsesLeavesSensitiveAndSmallResponsesAlone(t *testing.T) {
	large := jsonBody(8_000)
	cases := map[string]struct {
		request func() *http.Request
		handler http.HandlerFunc
	}{
		"peer network config": {func() *http.Request { return machineRequest(http.MethodPost, "/v1/peer-network/config") }, jsonResponder(large)},
		"machine peer network config": {func() *http.Request {
			return machineRequest(http.MethodPost, "/v1/machine-peer-network/config")
		}, jsonResponder(large)},
		"browser cookie": {func() *http.Request {
			r := machineRequest(http.MethodGet, "/v1/machines")
			r.Header.Set("Cookie", "session=x")
			return r
		}, jsonResponder(large)},
		"no credential": {func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/v1/machines", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			return r
		}, jsonResponder(large)},
		"gzip refused": {func() *http.Request {
			r := machineRequest(http.MethodGet, "/v1/machines")
			r.Header.Set("Accept-Encoding", "gzip;q=0, identity")
			return r
		}, jsonResponder(large)},
		"small body": {func() *http.Request { return machineRequest(http.MethodGet, "/v1/me") }, jsonResponder(`{"ok":true}`)},
		"not json": {func() *http.Request { return machineRequest(http.MethodGet, "/v1/file") }, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, large)
		}},
		"no content": {func() *http.Request { return machineRequest(http.MethodDelete, "/v1/machines/m") }, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := serveCompressed(t, tc.request(), tc.handler)
			if got := recorder.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want identity", got)
			}
		})
	}
}

// A status written before the body survives the buffering, and an API token qualifies like a machine proof.
func TestCompressResponsesKeepsStatusForAPITokens(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/machines", nil)
	r.Header.Set("Accept-Encoding", "br, gzip")
	r.Header.Set("Authorization", "Bearer token")
	recorder := serveCompressed(t, r, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, jsonBody(4_000))
	})
	if recorder.Code != http.StatusConflict || recorder.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, encoding %q", recorder.Code, recorder.Header().Get("Content-Encoding"))
	}
}
