package edgehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/addrlimit"
)

// Over the address limit, the edge answers 429 edge_rate_limited with Retry-After before any route
// or origin work.
func TestPolicyRefusesAnAddressOverItsLimit(t *testing.T) {
	policy := &Policy{addresses: addrlimit.New(addrlimit.Rate{PerSecond: 0.001, Burst: 1}, addressUpgradeRate, addrlimit.Streams)}
	request := func() (*httptest.ResponseRecorder, bool) {
		recorder := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "https://app.example.test/", nil)
		r.RemoteAddr = "192.0.2.9:4444"
		release, ok := policy.admitAddress(recorder, r)
		if ok {
			release()
		}
		return recorder, ok
	}
	if _, ok := request(); !ok {
		t.Fatal("first request refused")
	}
	second, ok := request()
	if ok || second.Code != http.StatusTooManyRequests || second.Header().Get("X-Paperboat-Error") != "edge_rate_limited" || second.Header().Get("Retry-After") == "" {
		t.Fatalf("limited request: ok=%v %d %q %q", ok, second.Code, second.Header().Get("X-Paperboat-Error"), second.Header().Get("Retry-After"))
	}
}

// X-43 saw 12-50% of 250 back-to-back new connections from one address reset: that is this limit (20 TLS handshakes/s,
// burst 50), not a preview fault. A browser opening 6 connections, even on several reloads, stays inside the burst.
func TestHandshakeRateAdmitsABrowserAndStopsAFlood(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := addrlimit.New(addressHandshakeRate, addrlimit.Rate{}, 0)
	l.SetClock(func() time.Time { return now })
	for i := 0; i < 36; i++ { // six reloads of six parallel connections
		if _, ok := l.Admit("192.0.2.9", false); !ok {
			t.Fatalf("browser handshake %d refused", i)
		}
	}
	l = addrlimit.New(addressHandshakeRate, addrlimit.Rate{}, 0)
	l.SetClock(func() time.Time { return now })
	admitted := 0
	for i := 0; i < 250; i++ { // 250 handshakes over 4 s
		now = now.Add(16 * time.Millisecond)
		if _, ok := l.Admit("192.0.2.9", false); ok {
			admitted++
		}
	}
	if admitted < 100 || admitted > 140 {
		t.Fatalf("flood admitted %d of 250, want about burst 50 + 20/s*4s", admitted)
	}
}
