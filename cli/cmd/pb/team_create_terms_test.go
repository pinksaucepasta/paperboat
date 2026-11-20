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

// Paying for a team needs the buyer's explicit agreement to the server's wording; the version sent is the one shown.
func TestTeamCreateNeedsAcceptedTerms(t *testing.T) {
	var created []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if teamFixtureUpdatePolicy(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/client-configuration":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"version": "1", "machines_url": "https://console.example.test/machines", "console_url": "https://console.example.test",
				"purchase_consent": map[string]any{"version": "2099-01-01", "text": "No refunds, canary wording."},
			}})
		case "POST /v1/teams":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			created = append(created, body)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"url": "https://checkout.example.test/pay"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)
	args := []string{"team", "create", "acme", "--no-wait", "--json", "--config", configPath, "--server", server.URL}

	var out bytes.Buffer
	if code := run(t.Context(), args, &out, &out); code == 0 || len(created) != 0 {
		t.Fatalf("created a team without the terms: exit=%d requests=%v", code, created)
	}
	if !strings.Contains(out.String(), "No refunds, canary wording.") || !strings.Contains(out.String(), "--accept-terms") {
		t.Fatalf("the refusal does not show the wording and the flag: %s", &out)
	}
	out.Reset()
	if code := run(t.Context(), append(args, "--accept-terms"), &out, &out); code != 0 {
		t.Fatalf("exit=%d output=%s", code, &out)
	}
	if len(created) != 1 || created[0]["terms_version"] != "2099-01-01" {
		t.Fatalf("the accepted version was not sent: %v", created)
	}
}
