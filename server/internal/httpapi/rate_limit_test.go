package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/ratelimit"
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
