package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/spf13/cobra"
)

func uninstallRecordCommand(t *testing.T, configPath string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "uninstall"}
	c.Flags().String("config", configPath, "")
	c.SetContext(t.Context())
	return c
}

// X-41: pb uninstall removes this machine's record on the server (the revoke call), so peers drop it; a
// computer that was never added makes no call.
func TestUninstallDisconnectsOwnMachineRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(dir, "identity"))
	var disconnects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/machines/mch_self/disconnect":
			disconnects.Add(1)
			writeAPIData(t, w, map[string]any{"disconnected": true})
		case "GET /v1/workspaces":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, srv.URL)
	c := uninstallRecordCommand(t, configPath)

	if err := disconnectOwnMachine(actionContext(c, nil)); err != nil || disconnects.Load() != 0 {
		t.Fatalf("never added: err=%v disconnects=%d", err, disconnects.Load())
	}
	store, err := identity.Open(identity.Config{StateRoot: filepath.Join(dir, "identity")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRegistration(identity.Registration{ServerURL: srv.URL, MachineID: "mch_self", EnvironmentID: "env_self", PublicKeyID: store.Current().ID, PublicIdentityKey: "test-public-key", InboxPath: dir, InstallationGeneration: 1, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if notice := removeOwnServerRecord(actionContext(c, nil)); notice != "" || disconnects.Load() != 1 {
		t.Fatalf("notice=%q disconnects=%d", notice, disconnects.Load())
	}
}

func TestUninstallServerRecordNoticeNamesTheConsole(t *testing.T) {
	original := disconnectOwnMachine
	t.Cleanup(func() { disconnectOwnMachine = original })
	disconnectOwnMachine = func(*command.Context) error {
		return &url.Error{Op: "Post", URL: "x", Err: errors.New("connection refused")}
	}
	notice := removeOwnServerRecord(nil)
	if !strings.Contains(notice, "could not be reached") || !strings.Contains(notice, "console") || !strings.Contains(notice, "continues locally") {
		t.Fatalf("notice = %q", notice)
	}
}
