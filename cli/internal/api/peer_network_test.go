package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestPeerNetworkAPIPathsAndStrictResponses(t *testing.T) {
	var unknown atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Idempotency-Key") != "network_operation_test" {
			t.Error("network request lost authentication or operation identity")
			w.WriteHeader(400)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["operation_id"] != "network_operation_test" {
			t.Error("invalid operation body")
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/v1/peer-network/register" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": PeerNetworkRegistrationResult{KeyGeneration: 1, VirtualAddress: "fd7a:115c:a1e0::1"}})
			return
		}
		if r.URL.Path != "/v1/peer-network/config" {
			t.Error("wrong config path")
			w.WriteHeader(404)
			return
		}
		data := map[string]any{"configuration": "signed-config", "candidate_set": "signed-candidates"}
		if unknown.Load() {
			data["unexpected_authority"] = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	c := New(srv.URL, config.Credential{AccessToken: "test-token"}, srv.Client())
	if r, err := c.RegisterPeerNetwork(context.Background(), PeerNetworkRegistration{OperationID: "network_operation_test"}); err != nil || r.KeyGeneration != 1 {
		t.Fatal("registration response failed", err)
	}
	if result, err := c.PeerNetworkConfiguration(context.Background(), "network_operation_test"); err != nil || result.Configuration != "signed-config" || result.CandidateSet != "signed-candidates" {
		t.Fatal("config response failed", err)
	}
	unknown.Store(true)
	if _, err := c.PeerNetworkConfiguration(context.Background(), "network_operation_test"); err == nil {
		t.Fatal("unknown security response field accepted")
	}
}
