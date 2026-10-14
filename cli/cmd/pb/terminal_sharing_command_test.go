package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type terminalSharingWriterError struct{}

func (terminalSharingWriterError) Error() string { return "private terminal-sharing writer failure" }

type terminalSharingFailWriter struct{ attempted bytes.Buffer }

func (w *terminalSharingFailWriter) Write(p []byte) (int, error) {
	_, _ = w.attempted.Write(p)
	return 0, terminalSharingWriterError{}
}

func TestTerminalSharingRejectsAmbiguousAudienceAndRole(t *testing.T) {
	for _, flags := range [][]string{{"--team", "team_1"}, {"--team", "team_1", "--all", "--member", "usr_2"}, {"--team", "team_1", "--all", "--role", "owner"}} {
		var out bytes.Buffer
		args := append([]string{"session", "share", "ses_1"}, flags...)
		if code := run(context.Background(), args, &out, &out); code != 2 || !strings.Contains(out.String(), "share requires") {
			t.Fatalf("code=%d output=%s", code, out.String())
		}
	}
}

// Task40 CLI compilation consumes the declared unsigned flag added by the
// concurrent Inbox task. Verify its value survives the existing command bridge.
func TestTerminalSharingBuildUnsignedFlagBridge(t *testing.T) {
	for _, value := range []string{"7", "-1"} {
		called := false
		tree := specTree(&command.Spec{Name: "fixture", Subcommands: []*command.Spec{{Name: "read", Flags: []command.Flag{&command.UintFlag{Name: "generation"}}, Action: func(c *command.Context) error {
			called = true
			if c.Uint("generation") != 7 {
				t.Fatal("generation lost")
			}
			return nil
		}}}}, "fixture")
		var output bytes.Buffer
		tree.SetOut(&output)
		tree.SetErr(&output)
		tree.SetArgs([]string{"read", "--generation", value})
		err := tree.ExecuteContext(context.Background())
		if value == "7" && (err != nil || !called) {
			t.Fatalf("unsigned bridge: %v", err)
		}
		if value == "-1" && (err == nil || called) {
			t.Fatal("negative generation reached action")
		}
	}
}

func TestTerminalSharingReturnsOutputErrorsBeforeReportingSuccessOrMutating(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	configPath := filepath.Join(root, "config.json")
	var mutations atomic.Int32
	state := map[string]any{
		"id": "ses_1", "name": "shared", "role": "owner", "can_manage": true,
		"target":  map[string]any{"name": "machine", "id": "mch_1", "machine_id": "mch_1", "machine_generation": 1},
		"sharing": map[string]any{"generation": 3, "team_id": "team_1", "audience": "selected_member"},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"sessions": []any{state}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/terminal-sessions/ses_1/sharing":
			writeAPIData(t, w, state)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/terminal-sessions/ses_1/sharing":
			mutations.Add(1)
			writeAPIData(t, w, state)
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

	var stderr bytes.Buffer
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restoreObserver()
	listingWriter := &terminalSharingFailWriter{}
	code := runWithReporter(context.Background(), []string{"--no-customization", "--config", configPath, "session", "shared"}, listingWriter, &stderr, nil)
	if code == 0 {
		t.Fatal("session listing reported success after stdout failed")
	}
	if strings.Contains(stderr.String(), "private terminal-sharing writer failure") || strings.Contains(listingWriter.attempted.String(), "private terminal-sharing writer failure") {
		t.Fatal("writer error text reached user-facing output")
	}
	if len(faults) != 1 {
		t.Fatalf("listing fault observations = %d, want one", len(faults))
	}
	if faults[0].Code != "unexpected_cli_failure" || !supportref.Valid(faults[0].SupportReference) || !slices.Contains(faults[0].ErrorChain, "terminalSharingWriterError") || strings.Contains(faults[0].Cause+faults[0].ErrorType+strings.Join(faults[0].ErrorChain, ","), "private terminal-sharing writer failure") {
		t.Fatalf("listing fault was not safely projected: %+v", faults[0])
	}

	var stdout bytes.Buffer
	var stderrWriter = &terminalSharingFailWriter{}
	faults = nil
	code = runWithReporter(context.Background(), []string{"--no-customization", "--config", configPath, "session", "share", "ses_1", "--team", "team_1", "--member", "usr_1"}, &stdout, stderrWriter, nil)
	if code == 0 {
		t.Fatal("share reported success after its safety notice failed to write")
	}
	if mutations.Load() != 0 {
		t.Fatalf("share mutation count = %d after safety notice failure", mutations.Load())
	}
	if strings.Contains(stdout.String(), "private terminal-sharing writer failure") || strings.Contains(stderrWriter.attempted.String(), "private terminal-sharing writer failure") {
		t.Fatal("writer error text reached user-facing output")
	}
	if len(faults) != 1 {
		t.Fatalf("share fault observations = %d, want one", len(faults))
	}
}
