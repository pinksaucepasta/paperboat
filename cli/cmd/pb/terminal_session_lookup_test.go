package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
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
		"terminal_offline_ab_12": "opened without Paperboat, so only `pb connect --session terminal_offline_ab_12` can attach it",
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
