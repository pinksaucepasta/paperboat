package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The pairing body is decoded as strictly as its siblings: unknown fields and trailing data fail.
func TestUserMachinePairingsRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	for _, body := range []string{`{"verifier":"v","extra":1}`, `{"verifier":"v"} {}`} {
		rec := httptest.NewRecorder()
		userMachinePairings(nil).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/machines/pairings", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "validation_failed") {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}
