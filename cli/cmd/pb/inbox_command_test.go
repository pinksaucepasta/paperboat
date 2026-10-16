package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestInboxPolicyAndDecisionsReachTheirAuthenticatedOwners(t *testing.T) {
	supportedTeamFixtureVersion(t)
	for _, action := range []string{"manual", "automatic", "approve", "decline"} {
		t.Run(action, func(t *testing.T) {
			policy := action == "manual" || action == "automatic"
			calls := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if teamFixtureUpdatePolicy(w, r) {
					return
				}
				calls++
				w.Header().Set("Content-Type", "application/json")
				if policy && r.Method == http.MethodGet && r.URL.Path == "/v1/team-inbox/policy" {
					_, _ = w.Write([]byte(`{"data":{"acceptance":"automatic","receipt_email":false,"generation":5}}`))
					return
				}
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					http.Error(w, "invalid body", 400)
					return
				}
				if policy {
					if r.Method != http.MethodPut || r.URL.Path != "/v1/team-inbox/policy" || input["acceptance"] != action || input["expected_generation"] != float64(5) || input["receipt_email"] != (action == "manual") {
						t.Error("policy did not preserve its selected acceptance, notification and generation")
						http.Error(w, "invalid policy", 400)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"acceptance": action, "receipt_email": action == "manual", "generation": 6}})
				} else {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/team-inbox/requests/request_1/"+action || input["expected_generation"] != float64(7) {
						t.Error("decision did not target the exact request and generation")
						http.Error(w, "invalid decision", 400)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"request_id": "request_1", "status": action + "d", "decision_generation": 8}})
				}
			}))
			defer backend.Close()
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			writeTestProfile(t, dir, configPath, backend.URL)
			args := []string{"inbox", "policy", action}
			expectedCalls := 2
			if policy && action == "manual" {
				args = append(args, "--receipt-email")
			}
			if !policy {
				args = []string{"inbox", action, "request_1", "--generation", "7"}
				expectedCalls = 1
			}
			args = append(args, "--json", "--server", backend.URL, "--config", configPath)
			var output, errors bytes.Buffer
			code := run(t.Context(), args, &output, &errors)
			if code != 0 || calls != expectedCalls {
				t.Fatalf("command rejected a valid Inbox operation: exit=%d requests=%d", code, calls)
			}
			var result struct {
				OK   bool           `json:"ok"`
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.OK || errors.Len() != 0 {
				t.Fatalf("command lost its success receipt: decode=%v", err)
			}
			if policy && (result.Data["acceptance"] != action || result.Data["receipt_email"] != (action == "manual") || result.Data["generation"] != float64(6)) {
				t.Fatal("policy response was not preserved")
			}
			if !policy && (result.Data["request_id"] != "request_1" || result.Data["decision_generation"] != float64(8)) {
				t.Fatal("decision response was not preserved")
			}
		})
	}
}
