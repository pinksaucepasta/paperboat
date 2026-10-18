package transactionalmail

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The provider boundary owns auth, exact payload/key preservation, safe retry
// classification and bounded responses. Durable scheduling is tested with PG.
func TestResendTransportContract(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		body            string
		accepted, retry bool
	}{
		{"accepted", 200, `{"id":"provider-accepted"}`, true, false},
		{"temporary", 503, `{"name":"service_unavailable"}`, false, true},
		{"throttled", 429, `{"name":"rate_limit_exceeded"}`, false, true},
		{"concurrent", 409, `{"name":"concurrent_idempotent_requests"}`, false, true},
		{"changed payload", 409, `{"name":"invalid_idempotent_request"}`, false, false},
		{"redirect", 307, ``, false, false},
		{"invalid sender", 403, `{"name":"validation_error"}`, false, false},
		{"truncated acceptance", 200, `{`, false, true},
		{"oversized", 200, strings.Repeat("x", 65537), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"from":"sender@example.test","to":["recipient@example.test"],"subject":"Invitation","html":"<p>Join</p>"}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != string(payload) || r.Method != "POST" || r.Header.Get("Idempotency-Key") != "mail-stable-key" || r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("provider request contract mismatch")
				}
				if tc.status == 307 {
					w.Header().Set("Location", "/redirect-target")
				}
				if tc.status == 429 {
					w.Header().Set("Retry-After", "7200")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			service := Service{Endpoint: server.URL, Key: "test-secret"}
			accepted, id, retry, retryAfter := service.send(context.Background(), "mail-stable-key", payload)
			if tc.status == 429 && retryAfter != 2*time.Hour {
				t.Fatal("provider Retry-After ignored")
			}
			if accepted != tc.accepted || retry != tc.retry || (accepted && id != "provider-accepted") {
				t.Fatalf("accepted=%v retry=%v", accepted, retry)
			}
		})
	}
}
