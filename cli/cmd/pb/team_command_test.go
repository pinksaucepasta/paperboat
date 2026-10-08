package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
