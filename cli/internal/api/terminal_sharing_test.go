package api

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTerminalSharingCleanupUsesStrictMutationEnvelope(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing client authentication")
		}
		if calls == 1 && (r.Method != http.MethodDelete || r.URL.Path != "/v1/terminal-sessions/ses_1/sharing") {
			t.Error("incorrect end-sharing route")
		}
		if calls == 2 && (r.Method != http.MethodPost || r.URL.Path != "/v1/terminal-sessions/ses_1/participants/usr_2/remove") {
			t.Error("incorrect participant removal route")
		}
		var body struct {
			OperationID string `json:"operation_id"`
			Generation  uint64 `json:"expected_generation"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.OperationID != "op_fixture" || body.Generation != 3 {
			t.Errorf("strict mutation failed: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": SharedTerminalSession{ID: "ses_1"}})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "fixture"}, server.Client())
	in := TerminalSharingMutation{OperationID: "op_fixture", ExpectedGeneration: 3}
	if _, err := client.EndTerminalSharing(context.Background(), "ses_1", in); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RemoveTerminalParticipant(context.Background(), "ses_1", "usr_2", in); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("cleanup requests not delivered")
	}
}
