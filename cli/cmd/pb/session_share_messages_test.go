package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// F3-06/N10: a session shared with you is listed and joined without knowing which workspace it is in:
// the account's every workspace is asked, and a session two workspaces answer for is refused with both.
func TestSharedSessionsAreFoundInEveryWorkspace(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	configPath := filepath.Join(root, "config.json")
	sharingIn := map[string]bool{"lanetv-n10": true}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workspace := r.URL.Query().Get("workspace")
		switch {
		case r.URL.Path == "/v1/workspaces":
			writeTestWorkspaces(t, w, "personal", "lanetv-n10", "other-team")
		case r.URL.Path == "/v1/terminal-sessions":
			sessions := []any{}
			if workspace == "lanetv-n10" {
				sessions = append(sessions, map[string]any{"id": "terminal_1", "name": "shared-one", "role": "interactive"})
			}
			writeAPIData(t, w, map[string]any{"sessions": sessions, "pagination": map[string]any{"limit": echoedLimit(r), "offset": 0, "total": len(sessions), "next_offset": nil}})
		case r.URL.Path == "/v1/terminal-sessions/terminal_1/sharing" && sharingIn[workspace]:
			writeAPIData(t, w, map[string]any{"id": "terminal_1", "role": "interactive"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	writeTestProfile(t, root, configPath, backend.URL)
	previousPaths := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) {
		return localapi.Paths{SocketPath: filepath.Join(root, "local-api.sock")}, nil
	}
	t.Cleanup(func() { currentLocalDaemonPaths = previousPaths })
	var out, stderr bytes.Buffer
	if code := runWithReporter(context.Background(), []string{"--no-customization", "--config", configPath, "session", "shared"}, &out, &stderr, nil); code != 0 || !strings.Contains(out.String(), "terminal_1") {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	client := api.New(backend.URL, config.Credential{AccessToken: "token"}, backend.Client())
	if err := client.SetWorkspace("personal"); err != nil {
		t.Fatal(err)
	}
	if got, err := sharedSessionWorkspace(context.Background(), client, "terminal_1"); err != nil || got != "lanetv-n10" {
		t.Fatalf("join workspace=%q err=%v", got, err)
	}
	if got, err := sharedSessionWorkspace(context.Background(), client, "terminal_missing"); err != nil || got != "personal" {
		t.Fatalf("missing session: workspace=%q err=%v, want the selected one so the server's not-found is shown", got, err)
	}
	sharingIn["other-team"] = true
	_, err := sharedSessionWorkspace(context.Background(), client, "terminal_1")
	if err == nil || !strings.Contains(err.Error(), "--workspace lanetv-n10") || !strings.Contains(err.Error(), "--workspace other-team") {
		t.Fatalf("ambiguous err=%v", err)
	}
	explicit := context.WithValue(context.Background(), workspaceExplicitKey{}, true)
	if got, err := sharedSessionWorkspace(explicit, client, "terminal_1"); err != nil || got != "personal" {
		t.Fatalf("explicit --workspace: workspace=%q err=%v", got, err)
	}
}

// writeTestWorkspaces answers /v1/workspaces with the personal workspace and the named teams.
func writeTestWorkspaces(t *testing.T, w http.ResponseWriter, ids ...string) {
	items := []map[string]string{}
	for _, id := range ids {
		kind, role := "team", "member"
		if id == "personal" {
			kind, role = "personal", "owner"
		}
		items = append(items, map[string]string{"id": id, "name": id, "kind": kind, "role": role})
	}
	writeAPIData(t, w, map[string]any{"items": items})
}

// A computer that was never added gets the next step, not a "repair the registration" message.
func TestMachineThatWasNeverAddedSaysToRunLogin(t *testing.T) {
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", t.TempDir())
	_, err := configuredMachineID()
	if err == nil {
		t.Fatal("a computer without a registration has a machine id")
	}
	var failure commandPreparationFailure
	if !errors.As(err, &failure) || failure.step != prepareMachineNotAdded || !strings.Contains(failure.Error(), "pb login") {
		t.Fatalf("err=%v", err)
	}
}
