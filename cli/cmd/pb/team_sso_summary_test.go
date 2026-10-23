package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// pb team sso status prints settings as words: on/off, "none" for no exemptions, and the
// session length in days (it printed "false", an empty list and "168h0m0s").
func TestSSOSummaryReadsAsSettings(t *testing.T) {
	var s api.TeamIdentity
	s.Seats, s.MinSeats = 10, 10
	var out bytes.Buffer
	if err := ssoSummary(&out, s); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Require SSO: off\nJIT provisioning: off\nSSO exempt: none\n") {
		t.Fatalf("defaults:\n%s", got)
	}
	s.SSO.Required, s.SSO.SessionSeconds, s.SSO.JIT, s.SSO.Exempt = true, 7*86400, true, []string{"a@example.test"}
	out.Reset()
	if err := ssoSummary(&out, s); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Require SSO: on, SSO session 7 days\nJIT provisioning: on\nSSO exempt: a@example.test\n") {
		t.Fatalf("configured:\n%s", got)
	}
	if got := ssoSession(36 * 3600); got != "36 hours" {
		t.Fatalf("ssoSession(36h) = %s", got)
	}
}

// The F19 refusals say what to do (live: an invite outside the verified domains printed only
// "The server rejected the request").
func TestTeamF19RefusalsExplainThemselves(t *testing.T) {
	for code, want := range map[string]string{
		"invitation_domain_not_verified": "verified domains",
		"team_sso_required":              "single sign-on",
		"directory_managed":              "company directory",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "server text", "request_id": "request_1"}})
		}))
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		writeTestProfile(t, dir, configPath, server.URL)
		var out bytes.Buffer
		status := run(context.Background(), []string{"team", "invite", "team_1", "x@example.test", "--generation", "1", "--config", configPath, "--server", server.URL}, &out, &out)
		server.Close()
		if status == 0 || !strings.Contains(out.String(), want) {
			t.Errorf("%s: code=%d output=%s", code, status, out.String())
		}
	}
}
