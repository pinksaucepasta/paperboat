package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"

	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// The handler accepts one JSON object with unknown fields ignored and nonce absent or a string; a service
// without a database answers 503 once the request is accepted, so 503 means the body passed.
func TestPortProbeRequestBody(t *testing.T) {
	handler := &SelfhostHandlers{Service: selfhost.New(nil, strings.Repeat("k", 32))}
	for body, want := range map[string]int{
		`{"transport":"tcp","port":7272}`:                                              503,
		`{"transport":"tcp","port":7272,"nonce":""}`:                                   400,
		`{"transport":"tcp","port":25000,"nonce":"abcdefghijklmnop0123","nonce":null}`: 400,
		`{"transport":"tcp","port":7272,"port":7272}`:                                  400,
		`{"transport":"udp","port":7274,"future":{"x":1}}`:                             503,
		`{"transport":"tcp","port":25000,"nonce":"abcdefghijklmnop0123"}`:              503,
		`{"transport":"tcp","port":7272,"nonce":null}`:                                 400,
		`{"transport":"tcp","port":7272,"nonce":5}`:                                    400,
		`{"transport":"tcp","port":7272} {"port":1}`:                                   400,
		`{"transport":"tcp","port":7272}x`:                                             400,
		`{"transport":"tcp","port":22}`:                                                400,
		`{"transport":"udp","port":7273}`:                                              400,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/selfhost/port-probe", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("c", 48))
		response := httptest.NewRecorder()
		handler.PortProbe(response, request)
		if response.Code != want {
			t.Errorf("%s: status %d, want %d (%s)", body, response.Code, want, response.Body)
		}
	}
}

// A node that probes past its limit gets 429 with Retry-After before any dial; another node is unaffected.
func TestPortProbeRateLimitedPerNode(t *testing.T) {
	handler := &SelfhostHandlers{Service: selfhost.New(nil, strings.Repeat("k", 32)), PortProbeLimit: ratelimit.New(1, 2, 1000)}
	probe := func(credential string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/selfhost/port-probe", strings.NewReader(`{"transport":"tcp","port":7272}`))
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		handler.PortProbe(response, request)
		return response
	}
	a, b := strings.Repeat("a", 48), strings.Repeat("b", 48)
	for i, want := range []int{503, 503, 429} {
		if got := probe(a); got.Code != want || want == 429 && got.Header().Get("Retry-After") == "" {
			t.Fatalf("probe %d: status %d, want %d", i, got.Code, want)
		}
	}
	if got := probe(b); got.Code != 503 {
		t.Fatalf("other node: status %d, want 503", got.Code)
	}
}
