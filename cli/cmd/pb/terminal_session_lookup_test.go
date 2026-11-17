package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// X-24: a session id Paperboat does not list used to fail with "Paperboat could not finish the command" and no step.
func TestUnknownTerminalSessionErrorNamesTheLookupAndTheRecovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "umts_1", "name": "shell-1"}},
			"pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
	}))
	defer srv.Close()
	client := api.New(srv.URL, config.Credential{}, srv.Client())
	for ref, want := range map[string]string{
		"nope":                   "not found in the sessions Paperboat lists for this machine",
		"terminal_offline_ab_12": "opened without Paperboat and is not registered yet (the next `pb connect --session terminal_offline_ab_12` while Paperboat answers registers it)",
	} {
		_, err := resolveTerminalSession(context.Background(), client, environmentTarget{id: "um_1"}, ref)
		if err == nil {
			t.Fatalf("%s: found a session that is not listed", ref)
		}
		message := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(message, want) || !strings.Contains(message, "pb session list") || strings.Contains(message, "could not finish the command") {
			t.Errorf("%s: message = %q (err %v), want %q and the recovery", ref, message, err, want)
		}
	}
}

// Resolving a session by name must not make the server probe every host: one round trip and one write per session.
func TestResolveTerminalSessionAsksForStoredRows(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "umts_1", "name": "shell-1"}},
			"pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
	}))
	defer srv.Close()
	client := api.New(srv.URL, config.Credential{}, srv.Client())
	session, err := resolveTerminalSession(context.Background(), client, environmentTarget{id: "um_1"}, "shell-1")
	if err != nil || session.ID != "umts_1" || !strings.Contains(query, "live=false") {
		t.Fatalf("session=%q err=%v query=%q, want live=false", session.ID, err, query)
	}
}
