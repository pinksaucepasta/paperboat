package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamDeleteRequiresExactConfirmationBeforeRequest(t *testing.T) {
	mutated := false
	supportedTeamFixtureVersion(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if teamFixtureUpdatePolicy(w, r) {
			return
		}
		mutated = true
		http.Error(w, "unexpected", 500)
	}))
	defer server.Close()
	var out bytes.Buffer
	code := run(context.Background(), []string{"team", "delete", "team_1", "--generation", "3", "--confirm", "wrong", "--server", server.URL}, &out, &out)
	if code != 2 || mutated || !strings.Contains(out.String(), "exact team identifier") {
		t.Fatalf("code=%d mutated=%t output=%q", code, mutated, out.String())
	}
}

func TestTeamUnknownSubcommandReturnsInvocationError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		helpArgs    []string
		helpText    string
		unknownArgs []string
		message     string
		helpCommand string
	}{
		{
			name:     "team",
			helpArgs: []string{"team"}, helpText: "Manage teams",
			unknownArgs: []string{"team", "not-a-team-action"},
			message:     "unknown team action", helpCommand: "pb team --help",
		},
		{
			name:     "team machine",
			helpArgs: []string{"team", "machine"}, helpText: "Share a personal enrollment",
			unknownArgs: []string{"team", "machine", "not-a-machine-action"},
			message:     "unknown team machine action", helpCommand: "pb team machine --help",
		},
		{
			name:     "env team",
			helpArgs: []string{"env", "team"}, helpText: "Manage encrypted ENV team scopes",
			unknownArgs: []string{"env", "team", "not-a-team-env-action"},
			message:     "unknown ENV team action", helpCommand: "pb env team --help",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if code := run(context.Background(), tc.helpArgs, &output, &output); code != 0 || !strings.Contains(output.String(), tc.helpText) {
				t.Fatalf("bare group help: code=%d output=%q", code, output.String())
			}

			output.Reset()
			code := run(context.Background(), tc.unknownArgs, &output, &output)
			if code != 2 || !strings.Contains(output.String(), tc.message) || !strings.Contains(output.String(), tc.helpCommand) {
				t.Fatalf("unknown group action: code=%d output=%q", code, output.String())
			}
		})
	}
}

func TestTeamGrantRequiresGenerationAndValidPermission(t *testing.T) {
	var out bytes.Buffer
	code := run(context.Background(), []string{"team", "grant", "team_1", "acct_2", "env", "env_1", "manage"}, &out, &out)
	if code != 2 || !strings.Contains(out.String(), "--generation") {
		t.Fatalf("code=%d output=%q", code, out.String())
	}
	out.Reset()
	code = run(context.Background(), []string{"team", "grant", "team_1", "acct_2", "env", "env_1", "manage", "--generation", "2"}, &out, &out)
	if code != 2 || !strings.Contains(out.String(), "read/write") {
		t.Fatalf("code=%d output=%q", code, out.String())
	}
}

func TestTeamDeclineInvitation(t *testing.T) {
	supportedTeamFixtureVersion(t)
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if teamFixtureUpdatePolicy(w, r) {
					return
				}
				calls++
				var body struct {
					OperationID string `json:"operation_id"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/team-invitations/inv_1/decline" || r.Header.Get("Authorization") == "" {
					t.Errorf("wrong invitation request: %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OperationID == "" || r.Header.Get("Idempotency-Key") != body.OperationID {
					t.Error("missing consistent operation identity")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusOK {
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"invitation_id": "inv_1", "team_id": "team_1", "state": "cancelled"}})
				} else {
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "invitation_conflict", "message": "Reload invitation."}})
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			writeTestProfile(t, dir, configPath, server.URL)
			var out bytes.Buffer
			code := run(context.Background(), []string{"team", "decline", "inv_1", "--json", "--config", configPath, "--server", server.URL}, &out, &out)
			if calls != 1 {
				t.Fatalf("decline calls=%d code=%d output=%s", calls, code, out.String())
			}
			if status != http.StatusOK {
				if code == 0 {
					t.Fatal("decline rejection reported success")
				}
				return
			}
			var result struct{ InvitationID, State string }
			var raw map[string]any
			if code != 0 || json.Unmarshal(out.Bytes(), &raw) != nil {
				t.Fatalf("code=%d output=%s", code, out.String())
			}
			result.InvitationID, _ = raw["invitation_id"].(string)
			result.State, _ = raw["state"].(string)
			if result.InvitationID != "inv_1" || result.State != "cancelled" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestTeamMachineGrantRejectsAmbiguousAudienceAndImplicitCapabilities(t *testing.T) {
	for _, flags := range [][]string{
		{"--capability", "terminal"},
		{"--all-members", "--member", "acct_2", "--capability", "terminal"},
		{"--all-members", "--capability", "publish"},
		{"--all-members", "--capability", "terminal,terminal"},
	} {
		var out bytes.Buffer
		args := append([]string{"team", "machine", "grant", "team_1", "machine_1", "--generation", "3"}, flags...)
		if code := run(context.Background(), args, &out, &out); code != 2 {
			t.Fatalf("%v: code=%d output=%s", flags, code, out.String())
		}
	}
}
func TestTeamMachineTransferRequiresExactConfirmationBeforeRequest(t *testing.T) {
	for _, action := range []string{"transfer-to-team", "remove"} {
		requested := false
		supportedTeamFixtureVersion(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if teamFixtureUpdatePolicy(w, r) {
				return
			}
			requested = true
			http.Error(w, "unexpected", 500)
		}))
		var out bytes.Buffer
		code := run(context.Background(), []string{"team", "machine", action, "team_1", "machine_1", "--generation", "3", "--confirm", "wrong", "--server", server.URL}, &out, &out)
		server.Close()
		if code != 2 || requested || !strings.Contains(out.String(), "exact machine identifier") {
			t.Fatalf("%s code=%d requested=%v output=%s", action, code, requested, out.String())
		}
	}
}

func supportedTeamFixtureVersion(t *testing.T) {
	previous := cliUpdateVersion
	t.Cleanup(func() { cliUpdateVersion = previous })
	cliUpdateVersion = func() string { return "2026.10.08.1" }
}
func teamFixtureUpdatePolicy(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/client/update-policy" {
		return false
	}
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"schema": "paperboat.client-update-policy/v1", "revision": 1, "minimum_version": "2026.09.05.0", "reason": "Supported fixture protocol"}})
	return true
}

type teamFailAtWriter struct {
	writes int
	failAt int
	err    error
	output bytes.Buffer
}

func (w *teamFailAtWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return w.output.Write(p)
}

func TestTeamHumanOutputReturnsWriterFailures(t *testing.T) {
	supportedTeamFixtureVersion(t)
	for _, tc := range []struct {
		name   string
		args   []string
		failAt int
	}{
		{name: "list row", args: []string{"team", "list"}, failAt: 1},
		{name: "ENV status", args: []string{"team", "get", "team_1"}, failAt: 2},
		{name: "ENV recovery guidance", args: []string{"team", "get", "team_1"}, failAt: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/client/update-policy":
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"schema": "paperboat.client-update-policy/v1", "revision": 1, "minimum_version": "2026.09.05.0", "reason": "Supported fixture protocol"}})
				case "/v1/teams":
					json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"team_id": "team_1", "owner_account": "acct_1", "generation": 4}}})
				case "/v1/teams/team_1":
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
						"env_status": "rotation_pending", "team_id": "team_1", "owner_account": "acct_1", "generation": 4,
						"deleted": false, "members": []any{}, "grants": []any{}, "machines": []any{}, "machine_grants": []any{},
					}})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			writeTestProfile(t, dir, configPath, server.URL)
			args := append([]string{}, tc.args...)
			args = append(args, "--config", configPath, "--server", server.URL)
			writer := &teamFailAtWriter{failAt: tc.failAt, err: errors.New("private team output canary")}
			if code := run(context.Background(), args, writer, writer); code == 0 {
				t.Fatalf("team command succeeded after output write %d failed", tc.failAt)
			}
			if strings.Contains(writer.output.String(), "private team output canary") {
				t.Fatal("writer error text escaped to user output")
			}
		})
	}
}
