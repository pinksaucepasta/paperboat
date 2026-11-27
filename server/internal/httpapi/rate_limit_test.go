package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
)

// F21-14 (#15): sign-in routes are limited per client network before the handler runs;
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

// A team pairing ~20 machines at once behind one network polls approval_pending every 2 s and may
// hit used/expired tokens: those refusals prove a real secret and must not use up the guess budget
// (they did, and pairing was refused with 429). Invalid secrets are still charged.
func TestSecretProvenRefusalsDoNotDrainPairingBudget(t *testing.T) {
	handler := signInLimit(ratelimit.New(30, 10, 1000), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Test-Code") {
		case "pending":
			writeError(w, r, http.StatusConflict, "user_machine_approval_pending", "pending")
		case "used":
			writeError(w, r, http.StatusConflict, "enrollment_token_used", "used")
		case "expired":
			writeError(w, r, http.StatusGone, "enrollment_token_expired", "expired")
		case "invalid":
			writeError(w, r, http.StatusBadRequest, "enrollment_token_invalid", "invalid")
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	send := func(path, code string) int {
		rec := httptest.NewRecorder()
		request := httptest.NewRequest("POST", path, nil)
		request.Header.Set("X-Test-Code", code)
		handler.ServeHTTP(rec, request)
		return rec.Code
	}
	for machine := range 20 {
		for poll := range 15 {
			if code := send("/v1/machines/pairings/installation", "pending"); code == http.StatusTooManyRequests {
				t.Fatalf("machine %d poll %d: legitimate pending poll refused with 429", machine+1, poll+1)
			}
		}
		for _, c := range []string{"used", "expired"} {
			if code := send("/v1/machines/pairings", c); code == http.StatusTooManyRequests {
				t.Fatalf("machine %d %s: refused with 429", machine+1, c)
			}
		}
		if code := send("/v1/machines/pairings", ""); code != http.StatusCreated {
			t.Fatalf("machine %d pairing: %d", machine+1, code)
		}
	}
	for range 10 {
		send("/v1/machines/pairings", "invalid")
	}
	if code := send("/v1/machines/pairings", "invalid"); code != http.StatusTooManyRequests {
		t.Fatalf("guessing after the burst: %d, want 429", code)
	}
}
