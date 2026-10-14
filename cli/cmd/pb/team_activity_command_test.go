package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamActivityEncodesCursorAndLimitAndPrintsJSON(t *testing.T) {
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/teams/team_1/activity" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		query = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"items":       []map[string]any{{"id": "aud_1", "actor_account": "acct_1", "action": "transfer", "created_at": "2026-09-12T12:00:00Z", "metadata": map[string]any{"new_owner": "acct_2"}}},
			"next_cursor": "next/+cursor==",
		}})
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)

	var out bytes.Buffer
	code := run(context.Background(), []string{"team", "activity", "team_1", "--cursor", "before/+cursor==", "--limit", "17", "--json", "--config", configPath, "--server", server.URL}, &out, &out)
	if code != 0 {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
	if query.Get("cursor") != "before/+cursor==" || query.Get("limit") != "17" {
		t.Fatalf("query=%q", query.Encode())
	}
	var page struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].Action != "transfer" || page.NextCursor != "next/+cursor==" {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
}

func TestTeamActivityRejectsInvalidLimitBeforeNetwork(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	var out bytes.Buffer
	code := run(context.Background(), []string{"team", "activity", "team_1", "--limit", "201", "--server", server.URL}, &out, &out)
	if code != 2 || requested || !strings.Contains(out.String(), "limit between 1 and 200") {
		t.Fatalf("code=%d requested=%t output=%q", code, requested, out.String())
	}
}

func TestTeamGetShowsActionablePendingENVStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"env_status": "rotation_pending", "team_id": "team_1", "owner_account": "acct_1", "generation": 4,
			"deleted": false, "members": []any{}, "grants": []any{}, "machines": []any{}, "machine_grants": []any{},
		}})
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)
	var out bytes.Buffer
	if code := run(context.Background(), []string{"team", "get", "team_1", "--config", configPath, "--server", server.URL}, &out, &out); code != 0 {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
	if text := out.String(); !strings.Contains(text, "ENV\trotation_pending") || !strings.Contains(text, "authorized ENV keys") || !strings.Contains(text, "pb env team rotate <team>") {
		t.Fatalf("pending ENV output=%q", text)
	}
}
