package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestEnrollmentUsesCurrentV1Contract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machine-enrollments" {
			t.Errorf("unexpected enrollment request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Idempotency-Key") != "release-contract-test" {
			t.Error("missing operation identity")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 0 {
			t.Errorf("enrollment must not send obsolete role or shell fields: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"ume_test","bootstrap_token":"test-only-token","server_url":"https://api.example.test"}}`))
	}))
	defer server.Close()
	result, err := New(server.URL, config.Credential{AccessToken: "test-only-access", TokenType: "Bearer"}, server.Client()).StartMachineEnrollment(context.Background(), "release-contract-test")
	if err != nil || result.ID != "ume_test" {
		t.Fatalf("enrollment result=%#v err=%v", result, err)
	}
}
