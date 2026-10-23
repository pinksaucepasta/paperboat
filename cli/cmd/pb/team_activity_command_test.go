package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeamActivityEncodesCursorAndLimitAndPrintsJSON(t *testing.T) {
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/activity" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		query = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"items":       []map[string]any{{"id": "aud_1", "occurred_at": "2026-09-12T12:00:00Z", "workspace": "team:team_1", "action": "team.transfer", "result": "success", "actor": map[string]any{"kind": "user", "id": "acct_1"}, "target": map[string]any{"kind": "team", "id": "team_1"}, "details": map[string]any{"new_owner": "acct_2"}}},
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
	if query.Get("cursor") != "before/+cursor==" || query.Get("limit") != "17" || query.Get("workspace") != "team_1" {
		t.Fatalf("query=%q", query.Encode())
	}
	var page struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].Action != "team.transfer" || page.NextCursor != "next/+cursor==" {
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

func TestTeamActivityFiltersAndExport(t *testing.T) {
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		if r.URL.Path != "/v1/activity/export" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("id,occurred_at\naud_1,2026-09-12T12:00:00Z\n"))
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)
	file := filepath.Join(dir, "audit.csv")
	var out bytes.Buffer
	code := run(context.Background(), []string{"team", "activity", "export", "team_1", "--format", "csv", "--result", "denied", "--action", "team", "--since", "7d", "-o", file, "--config", configPath, "--server", server.URL}, &out, &out)
	if code != 0 {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
	q := queries[0]
	if q.Get("workspace") != "team_1" || q.Get("format") != "csv" || q.Get("result") != "denied" || q.Get("action") != "team" || q.Get("since") == "" {
		t.Fatalf("query=%q", q.Encode())
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.HasPrefix(string(data), "id,occurred_at") {
		t.Fatalf("file=%q err=%v", data, err)
	}
	out.Reset()
	if code = run(context.Background(), []string{"team", "activity", "team_1", "--result", "maybe", "--server", server.URL}, &out, &out); code != 2 {
		t.Fatalf("bad result accepted: %d %s", code, out.String())
	}
}

func TestActivityTimeAcceptsDaysDurationsAndRFC3339(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{"7d": now.Add(-7 * 24 * time.Hour), "90m": now.Add(-90 * time.Minute), "2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)} {
		if got, err := activityTime(in, now); err != nil || !got.Equal(want) {
			t.Errorf("%s = %v err=%v", in, got, err)
		}
	}
	if _, err := activityTime("yesterday", now); err == nil {
		t.Fatal("yesterday accepted")
	}
}
