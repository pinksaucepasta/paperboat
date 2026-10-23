package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// A descriptor request the server refused with 503 (a serialization conflict under 40 concurrent execs,
// Lane W / support_45f4b28f) is asked again with backoff; other refusals are final, and repeats are
// bounded.
func TestDescriptorIssuanceRetriesServiceUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int
		wantCalls int32
		wantErr   bool
	}{
		{"recovers", []int{503, 429, 200}, 3, false},
		{"final refusal", []int{403}, 1, true},
		{"bounded", []int{503, 503, 503, 503, 503, 503, 503}, issuanceAttempts, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				status := tc.statuses[min(int(n), len(tc.statuses))-1]
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status != 200 {
					_, _ = w.Write([]byte(`{"error":{"code":"credential_unavailable","message":"x"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{}}`))
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
			var out map[string]any
			err := client.doIssuance(context.Background(), http.MethodPost, "/v1/machines/m/exec-descriptor", map[string]string{"operation_id": "operation_1"}, &out, false)
			var refused *APIError
			if calls.Load() != tc.wantCalls || (err != nil) != tc.wantErr || tc.wantErr && !errors.As(err, &refused) {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
