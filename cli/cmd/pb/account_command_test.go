package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// C263: deleting an account is console-only (user decision 2026-10-23); pb prints the link.
func TestAccountDeleteOnlyPrintsTheConsoleLink(t *testing.T) {
	link, err := accountDeletionLink("https://console.example.test/")
	if err != nil || link != "https://console.example.test/settings/account" {
		t.Fatalf("link=%q err=%v", link, err)
	}
	for _, bad := range []string{"", "ftp://console.example.test", "https://user@console.example.test"} {
		if _, err := accountDeletionLink(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if delete := accountCobraCommand().Commands()[0]; delete.Flags().Lookup("yes") != nil {
		t.Fatal("pb account delete must not take --yes")
	}
	refused := api.APIError{Code: "account_deletion_console_only"}
	if refused.PublicCode() != "account_deletion_console_only" {
		t.Fatal("console-only refusal is not a public code")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "account_deletion_console_only", "message": "raw", "details": map[string]any{"console_url": "https://console.example.test/settings/account"}}})
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	_, err = client.ListWorkspaces(t.Context())
	var out bytes.Buffer
	out.WriteString(commandFailureMessage(err, classifyCommandFailure(err)))
	if !strings.Contains(out.String(), "https://console.example.test/settings/account") {
		t.Fatalf("refusal message %q", out.String())
	}
}

// `pb version` prints the version instead of looking for a machine named "version" (Lane O).
func TestVersionIsACommand(t *testing.T) {
	c := versionCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs([]string{"--json"})
	if err := c.Execute(); err != nil || !strings.Contains(out.String(), `"version"`) {
		t.Fatalf("out=%q err=%v", out.String(), err)
	}
}
