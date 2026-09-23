package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestSetupMachineUsesCanonicalAliasContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/setup" {
			http.NotFound(w, r)
			return
		}
		// Match the server's strict v1 request: obsolete display_name/setup_mode
		// fields must not cross the authenticated setup boundary.
		var body struct {
			Alias             string            `json:"alias"`
			Platform          string            `json:"platform"`
			Architecture      string            `json:"architecture"`
			WorkspaceRoot     string            `json:"workspace_root"`
			PublicIdentityKey string            `json:"public_identity_key"`
			RuntimeVersions   map[string]string `json:"runtime_versions"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			t.Errorf("invalid setup wire contract: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.Alias != "studio" || body.PublicIdentityKey != "existing-machine-key" || body.WorkspaceRoot != "/Users/studio" || body.RuntimeVersions["pb"] != "test-build" {
			t.Errorf("setup identity or alias changed: alias=%q", body.Alias)
			http.Error(w, "invalid identity", http.StatusBadRequest)
			return
		}
		writeData(w, http.StatusOK, UserMachine{ID: "existing-machine"})
	}))
	defer server.Close()
	got, err := New(server.URL, config.Credential{AccessToken: "fixture-token"}, nil).SetupMachine(context.Background(), MachineSetupInput{SetupMode: "host", Alias: " Studio ", Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/studio", PublicIdentityKey: "existing-machine-key", RuntimeVersions: map[string]string{"pb": "test-build"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "existing-machine" {
		t.Fatal("existing machine identity lost")
	}
}
