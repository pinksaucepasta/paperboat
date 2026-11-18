package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Trailing data fails the pairing body; a field a newer pb adds is ignored.
func TestUserMachinePairingsRejectsTrailingData(t *testing.T) {
	for _, body := range []string{`{"verifier":"v"} {}`} {
		rec := httptest.NewRecorder()
		userMachinePairings(nil).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/machines/pairings", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "validation_failed") {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}
