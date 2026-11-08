package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
)

// F21-14 (Lane X #15): sign-in routes are limited per client network before the handler runs;
// other routes are not.
func TestSignInLimitAppliesToSignInRoutesOnly(t *testing.T) {
	reached := 0
	handler := signInLimit(ratelimit.New(60, 2, 1000), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	for i := range 2 {
		if rec := send("POST", "/v1/operator/login"); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: %d", i+1, rec.Code)
		}
	}
	rec := send("POST", "/v1/operator/login")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || reached != 2 {
		t.Fatalf("over the burst: %d retry-after=%q reached=%d", rec.Code, rec.Header().Get("Retry-After"), reached)
	}
	if rec := send("POST", "/v1/auth/device/authorize"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("other sign-in route from the same network: %d", rec.Code)
	}
	if rec := send("GET", "/v1/machines"); rec.Code != http.StatusNoContent {
		t.Fatalf("a non-sign-in route was limited: %d", rec.Code)
	}
}

// Secret-exchange routes (pairing, helper enrollment, guest join code and ticket) charge the
// network only for rejected attempts, and guesses spread across terminal sessions share one budget.
func TestFailedSecretExchangesAreLimitedPerNetwork(t *testing.T) {
	handler := signInLimit(ratelimit.New(60, 2, 1000), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Reject") != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(path string, reject bool) int {
		rec := httptest.NewRecorder()
		request := httptest.NewRequest("POST", path, nil)
		if reject {
			request.Header.Set("X-Test-Reject", "1")
		}
		handler.ServeHTTP(rec, request)
		return rec.Code
	}
	for i := range 5 {
		if code := send("/v1/helper-enrollments", false); code != http.StatusNoContent {
			t.Fatalf("success %d was limited: %d", i+1, code)
		}
	}
	if code := send("/v1/terminal-sessions/ts_a/code-join", true); code != http.StatusNotFound {
		t.Fatalf("first guess: %d", code)
	}
	if code := send("/v1/terminal-sessions/ts_b/guest-browser-ticket", true); code != http.StatusNotFound {
		t.Fatalf("second guess on another session: %d", code)
	}
	for _, path := range []string{"/v1/terminal-sessions/ts_c/code-join", "/v1/machines/pairings", "/v1/helper-identity-renewals", "/v1/machines/pairings/reset", "/v1/machines/pairings/installation"} {
		if code := send(path, false); code != http.StatusTooManyRequests {
			t.Fatalf("%s after the failures: %d, want 429", path, code)
		}
	}
	if code := send("/v1/terminal-sessions/ts_c/other", false); code != http.StatusNoContent {
		t.Fatalf("an unrelated terminal route was limited: %d", code)
	}
}
