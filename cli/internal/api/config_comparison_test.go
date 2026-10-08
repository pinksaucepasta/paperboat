package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
)

func TestConfigComparisonDescriptorChecksExactReadBinding(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "foreign path"}[foreign], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/config-sync/environments/env_1/conflicts/conflict_1/connection" {
					t.Errorf("request %s", r.URL)
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if body["source_machine_id"] != "source" || body["cli_client_session_id"] != "cli" || body["path"] != "settings.txt" || body["expected_remote_revision"] != "remote" {
					t.Errorf("missing exact read authority: %v", body)
				}
				expiry := time.Now().Add(time.Minute)
				binding := configsync.ConflictComparisonRequest{AssignmentID: "assignment_1", AssignmentVersion: 3, Path: "settings.txt", ConflictRevision: "conflict_1", ExpectedRemoteRevision: "remote"}
				if foreign {
					binding.Path = "other"
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"operation_id": "operation_compare_1", "environment": map[string]any{"id": "env_1", "kind": "machine", "resource_id": "machine_1", "state": "ready", "root": "/home/user"}, "endpoints": map[string]string{"quic": "quic://host.test:443", "wss": "wss://host.test/v1/runtime"}, "auth": map[string]any{"method": "bearer", "token": "opaque", "access_session_id": "token_compare_1", "expires_at": expiry, "scopes": []string{"config:compare"}}, "expires_at": expiry, "binding": binding}})
			}))
			defer server.Close()
			_, err := New(server.URL, config.Credential{}, server.Client()).ConfigConflictConnection(context.Background(), "env_1", "conflict_1", 3, "settings.txt", "remote", "source", "cli")
			if (err != nil) != foreign {
				t.Fatalf("foreign=%v err=%v", foreign, err)
			}
		})
	}
}
