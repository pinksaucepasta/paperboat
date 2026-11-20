package edgehttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

// freeTestLimits is a limiter with the server's default Free rates and one fake clock for both tables.
func freeTestLimits(now *time.Time) *FreeRequestLimits {
	l := NewFreeRequestLimits()
	rates := edgelimits.Default().FreeRequests
	rates.PricingURL = "https://site.example.test/pricing"
	l.Set(rates)
	t := l.tables.Load()
	t.perAddress.SetClock(func() time.Time { return *now })
	t.perPublication.SetClock(func() time.Time { return *now })
	return l
}

// A first load of an unbundled dev server (1,200 module requests at once) passes; so do 1,500.
func TestFreeRequestLimitsAdmitAFirstLoadBurst(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := freeTestLimits(&now)
	for i := 0; i < 1500; i++ {
		if l.over("app.preview.example.test", "192.0.2.1") != nil {
			t.Fatalf("first-load request %d refused", i)
		}
	}
	if l.over("app.preview.example.test", "192.0.2.1") == nil {
		t.Fatal("request past the burst admitted")
	}
}

// One address holding 20 requests/s against a 10/s limit is refused once its burst is spent, and keeps getting about 10/s.
func TestFreeRequestLimitsSlowOneAddressSustainedAboveItsRate(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := freeTestLimits(&now)
	admitted, refused := 0, 0
	for i := 0; i < 20*400; i++ { // 400 s at 20/s
		now = now.Add(50 * time.Millisecond)
		if l.over("app.preview.example.test", "192.0.2.1") != nil {
			refused++
		} else {
			admitted++
		}
	}
	if refused < 1500 || admitted > 1500+10*400+10 {
		t.Fatalf("admitted %d, refused %d: want the burst plus about 10/s", admitted, refused)
	}
	// Another visitor of the same link is untouched by it.
	if l.over("app.preview.example.test", "192.0.2.2") != nil {
		t.Fatal("a different visitor was refused")
	}
}

// The publication's own cap trips across many addresses that each stay inside their rate, and spares other publications.
func TestFreeRequestLimitsCapAPublicationAcrossManyAddresses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := freeTestLimits(&now)
	admitted := 0
	for i := 0; i < 5000; i++ { // 500 addresses, 10 requests each: well inside the per-address burst
		if l.over("busy.preview.example.test", fmt.Sprintf("198.51.%d.%d", i/250, i%250)) == nil {
			admitted++
		}
	}
	if admitted != 3000 {
		t.Fatalf("admitted %d of 5000, want the publication burst of 3000", admitted)
	}
	if l.over("quiet.preview.example.test", "198.51.100.1") != nil {
		t.Fatal("another publication was refused")
	}
	now = now.Add(time.Second)
	got := 0
	for i := 0; i < 100; i++ {
		if l.over("busy.preview.example.test", fmt.Sprintf("203.0.%d.1", i)) == nil {
			got++
		}
	}
	if got != 50 {
		t.Fatalf("admitted %d after one second, want the sustained 50", got)
	}
}

// Through the whole policy: a Free host is limited, a Paid host and an edge told nothing are not, a 1,200-request first load
// passes the edge floor and the Free limit, and the refusal says plainly what limited it.
func TestPolicyAppliesFreeRequestLimitsOnlyToFreeHostsOfAnEdgeThatWasTold(t *testing.T) {
	serve := func(policy *Policy, host, address string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host, request.RemoteAddr = host, address+":5555"
		request.Header.Set("Accept", "text/html")
		policy.ServeHTTP(recorder, request)
		return recorder
	}
	build := func(limits *FreeRequestLimits) *Policy {
		policy, err := New(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 1024,
			Readiness:      readinessFunc(func(string) (string, string, bool) { return "ready", "", true }),
			RequestLimited: func(host string) bool { return host == "free.preview.example.test" },
			FreeRequests:   limits,
		}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		if err != nil {
			t.Fatal(err)
		}
		return policy
	}
	told := NewFreeRequestLimits()
	rates := edgelimits.Default().FreeRequests
	rates.PricingURL = "https://site.example.test/pricing"
	told.Set(rates)
	policy := build(told)
	for i := 0; i < 1200; i++ {
		if got := serve(policy, "free.preview.example.test", "192.0.2.1"); got.Code == http.StatusTooManyRequests {
			t.Fatalf("first-load request %d of 1200 refused: %s", i, got.Header().Get("X-Paperboat-Error"))
		}
	}
	var refused *httptest.ResponseRecorder
	for i := 0; i < 400 && refused == nil; i++ {
		if got := serve(policy, "free.preview.example.test", "192.0.2.1"); got.Code == http.StatusTooManyRequests {
			refused = got
		}
	}
	if refused == nil || refused.Header().Get("X-Paperboat-Error") != "free_plan_rate_limited" || refused.Header().Get("Retry-After") == "" {
		t.Fatalf("no Free-plan refusal after the burst: %+v", refused)
	}
	body := refused.Body.String()
	for _, want := range []string{"Free plan", "10 requests per second", "50 per second", "https://site.example.test/pricing"} {
		if !strings.Contains(body, want) {
			t.Errorf("429 page lacks %q", want)
		}
	}
	// The same visitor, on a host the server did not list as Free, is never limited by it.
	for i := 0; i < 600; i++ {
		if got := serve(policy, "paid.preview.example.test", "192.0.2.1"); got.Code == http.StatusTooManyRequests {
			t.Fatalf("paid host request %d refused: %s", i, got.Header().Get("X-Paperboat-Error"))
		}
	}
	// A customer-owned edge is sent no rates: the same Free host is never limited there.
	untold := build(NewFreeRequestLimits())
	for i := 0; i < 5000; i++ {
		if got := serve(untold, "free.preview.example.test", fmt.Sprintf("198.51.%d.%d", i/250, i%250)); got.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d refused on an edge that was told no limits", i)
		}
	}
	told.Set(edgelimits.FreeRequests{}) // the server later sends none (the node became customer-owned)
	if got := serve(policy, "free.preview.example.test", "192.0.2.1"); got.Code == http.StatusTooManyRequests {
		t.Fatal("limit survived the server withdrawing it")
	}
}
