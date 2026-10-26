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

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// F3-06: an empty `pb session shared` says where team-shared sessions are, instead of printing nothing.
func TestSessionSharedExplainsWhereTeamSharedSessionsAre(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	configPath := filepath.Join(root, "config.json")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/terminal-sessions" {
			writeAPIData(t, w, map[string]any{"sessions": []any{}, "pagination": map[string]any{"limit": echoedLimit(r), "offset": 0, "total": 0, "next_offset": nil}})
			return
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()
	writeTestProfile(t, root, configPath, backend.URL)
	previousPaths := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) {
		return localapi.Paths{SocketPath: filepath.Join(root, "local-api.sock")}, nil
	}
	t.Cleanup(func() { currentLocalDaemonPaths = previousPaths })
	var out, stderr bytes.Buffer
	if code := runWithReporter(context.Background(), []string{"--no-customization", "--config", configPath, "session", "shared"}, &out, &stderr, nil); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	if out.Len() != 0 || !strings.Contains(stderr.String(), "--workspace <team>") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
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
