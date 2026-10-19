package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Owners and admins accept, decline and list machines waiting to join a team, and switch the rule.
func TestTeamMachineAdmissionCommands(t *testing.T) {
	supportedTeamFixtureVersion(t)
	var actions []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if teamFixtureUpdatePolicy(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		team := map[string]any{"team_id": "team_1", "owner_account": "owner_1", "generation": 7, "members": []any{}, "grants": []any{}, "machines": []any{}, "machine_grants": []any{}, "terminal_session_grants": []any{}, "approve_new_machines": true,
			"machine_requests": []map[string]any{{"machine_id": "machine_9", "alias": "studio", "requested_by": "member_1", "state": "pending", "requested_at": "2026-10-19T00:00:00Z"}}}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/teams/team_1":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": team})
		case r.Method == http.MethodPost && (r.URL.Path == "/v1/teams/team_1/machines" || r.URL.Path == "/v1/teams/team_1/actions"):
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			input["path"] = r.URL.Path
			actions = append(actions, input)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": team})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)
	run1 := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if code := run(t.Context(), append(args, "--server", server.URL, "--config", configPath), &out, &out); code != 0 {
			t.Fatalf("%v exit=%d\n%s", args, code, out.String())
		}
		return out.String()
	}
	if out := run1("team", "machine", "requests", "team_1"); !strings.Contains(out, "machine_9") || !strings.Contains(out, "studio") || !strings.Contains(out, "pending") {
		t.Fatalf("requests=%q", out)
	}
	if out := run1("team", "machine", "approve", "team_1", "machine_9"); !strings.Contains(out, "Accepted machine machine_9") {
		t.Fatalf("approve=%q", out)
	}
	if out := run1("team", "machine", "decline", "team_1", "machine_9", "--generation", "7"); !strings.Contains(out, "Declined machine machine_9") {
		t.Fatalf("decline=%q", out)
	}
	if out := run1("team", "machine", "approval", "team_1", "off"); !strings.Contains(out, "join right away") {
		t.Fatalf("approval=%q", out)
	}
	if len(actions) != 3 || actions[0]["action"] != "approve" || actions[0]["expected_generation"] != float64(7) || actions[0]["machine_id"] != "machine_9" || actions[1]["action"] != "decline" || actions[2]["action"] != "machine_approval" || actions[2]["approve_new_machines"] != false {
		t.Fatalf("actions=%v", actions)
	}
}
