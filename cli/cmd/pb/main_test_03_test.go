package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/statusbar"
	"github.com/pinksaucepasta/paperboat/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func (r *execChunkReader) Read(value []byte) (int, error) {
	if r.index == len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	return copy(value, chunk), nil
}

func TestForwardExecInputSwitchesConnectionsWithoutReplayingUncertainChunk(t *testing.T) {
	first := &execInputTestConn{writeErr: tunnel.ErrTransportLost}
	second := &execInputTestConn{}
	connections := newExecConnectionRef(first)
	done := make(chan struct{})
	go func() {
		forwardExecInput(context.Background(), &execChunkReader{chunks: [][]byte{[]byte("uncertain"), []byte("next")}}, connections)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for connections.Current() != nil {
		if time.Now().After(deadline) {
			t.Fatal("failed connection was not cleared")
		}
		time.Sleep(time.Millisecond)
	}
	if err := connections.Set(second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("input forwarding did not finish")
	}
	if len(first.writes) != 1 || string(first.writes[0]) != "uncertain" {
		t.Fatalf("first writes = %q", first.writes)
	}
	if len(second.writes) != 1 || string(second.writes[0]) != "next" || !second.closeWrite {
		t.Fatalf("second writes = %q closeWrite=%v", second.writes, second.closeWrite)
	}
}

func TestFinishExecResultReservedExitCodesAndJSONTransportFailure(t *testing.T) {
	for name, test := range map[string]struct {
		code         int
		err          error
		sawTerminal  bool
		wantExit     int
		wantJSONCode string
	}{
		"remote exit":       {code: 37, wantExit: 37, sawTerminal: true},
		"timeout":           {err: &tunnel.RemoteExecError{Code: "exec_timeout"}, wantExit: 124, sawTerminal: true},
		"cancel":            {err: &tunnel.RemoteExecError{Code: "exec_canceled"}, wantExit: 130, sawTerminal: true},
		"remote failure":    {err: &tunnel.RemoteExecError{Code: "exec_result_unavailable"}, wantExit: 125, sawTerminal: true},
		"transport failure": {err: tunnel.ErrTransportLost, wantExit: 255, wantJSONCode: "transport_lost"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := finishExecResult(&stdout, &stderr, "exec-operation-1", true, test.sawTerminal, test.code, test.err)
			var exit interface{ ExitCode() int }
			if !errors.As(err, &exit) || exit.ExitCode() != test.wantExit {
				t.Fatalf("error=%v exit=%v, want %d", err, exit, test.wantExit)
			}
			if stderr.Len() != 0 {
				t.Fatalf("JSON mode wrote stderr: %q", stderr.String())
			}
			if test.wantJSONCode == "" {
				if stdout.Len() != 0 {
					t.Fatalf("unexpected JSON: %q", stdout.String())
				}
				return
			}
			var event struct {
				Event     string `json:"event"`
				ErrorCode string `json:"error_code"`
				Changed   bool   `json:"changed"`
				Uncertain bool   `json:"uncertain"`
			}
			if json.Unmarshal(stdout.Bytes(), &event) != nil || event.Event != "failed" || event.ErrorCode != test.wantJSONCode || !event.Changed || !event.Uncertain {
				t.Fatalf("event=%#v output=%q", event, stdout.String())
			}
		})
	}
}

func TestExecEventNameReportsSignalTerminalEvent(t *testing.T) {
	event := tunnel.ExecEvent{State: "exited", Result: &tunnel.ExecResult{Signal: "SIGTERM"}}
	if got := execEventName(event); got != "signaled" {
		t.Fatalf("event name = %q", got)
	}
	changed := execEventChanged(event)
	if changed == nil || !*changed {
		t.Fatalf("changed = %v", changed)
	}
}

func TestCollectLocalDoctorDoesNotCreateUnconfiguredIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-state")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", root)
	report := collectLocalDoctor(t.Context())
	if report.SetupState != "not_set_up" || report.IdentityState != "missing" || !slices.Contains(report.RecoveryActions, "run pb setup") {
		t.Fatalf("report=%+v", report)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor mutated unconfigured state: %v", err)
	}
}

func TestCollectLocalDoctorReportsMachineInboxCredential(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", root)
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	inboxPath := filepath.Join(root, "Paperboat Inbox")
	if err := inbox.EnsurePath(inboxPath); err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	registration := identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_local", EnvironmentID: "env_local", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: inboxPath, InstallationGeneration: 4, UpdatedAt: time.Now().UTC()}
	if err := store.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMachineControl(identity.MachineControl{MachineID: registration.MachineID, EnvironmentID: registration.EnvironmentID, InstallationGeneration: registration.InstallationGeneration, Credential: strings.Repeat("x", 32), ExpiresAt: time.Now().UTC().Add(time.Hour), KeyID: key.ID}); err != nil {
		t.Fatal(err)
	}
	report := collectLocalDoctor(t.Context())
	if report.SetupState != "configured" || report.IdentityState != "valid" || report.MachineID != "machine_local" || report.InstallationGeneration != 4 || report.InboxState != "ready" || report.CredentialState != "valid" {
		t.Fatalf("report=%+v", report)
	}
}

func TestCLIExposesDaemonWithNestedRuntimeEntryPoints(t *testing.T) {
	root := newRootCommand()
	daemon, _, err := root.Find([]string{"daemon"})
	if err != nil || daemon == root || daemon.Hidden {
		t.Fatalf("explicit daemon command missing: %v", err)
	}
	worker, _, err := root.Find([]string{"daemon", "__runtime-worker"})
	if err != nil || worker.Parent() != daemon {
		t.Fatalf("worker is not owned by daemon: %v", err)
	}
	for _, command := range root.Commands() {
		if command.Name() == "__local-daemon" || command.Name() == "__windows-sshd-service" || strings.HasPrefix(command.Name(), "__runtime-") && command.Name() != "__runtime-service" {
			t.Fatalf("CLI exposes daemon runtime %s", command.Name())
		}
	}
}

func TestDaemonHelpThroughCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"daemon", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "pb daemon") || stderr.Len() != 0 {
		t.Fatalf("daemon help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestConnectTelemetryFailsOpenWithWarning(t *testing.T) {
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{EventLogPath: filepath.Join(blockedParent, "telemetry.jsonl")}}
	var warnings bytes.Buffer
	sink, closeSink := connectTelemetry(cfg, &warnings)
	defer closeSink()
	if _, ok := sink.(telemetry.NopSink); !ok {
		t.Fatalf("sink type = %T, want telemetry.NopSink", sink)
	}
	message := warnings.String()
	const prefix = "warning: local event log unavailable; check its storage permissions. Support reference: "
	reference := strings.TrimSuffix(strings.TrimPrefix(message, prefix), ".\n")
	if !strings.HasPrefix(message, prefix) || !supportref.Valid(reference) || strings.Contains(message, blockedParent) {
		t.Fatalf("warning = %q", message)
	}
}

func TestRetryableInitialConnectError(t *testing.T) {
	if retryableInitialConnectError(fmt.Errorf("connect to machine: %w", resolver.ErrMachineNotFound)) {
		t.Fatal("machine lookup failure must not retry")
	}
	if !retryableInitialConnectError(&api.APIError{Code: "machine_not_ready"}) {
		t.Fatal("machine_not_ready should retry")
	}
}

func TestSelectTerminalSessionRejectsAmbiguousMachineAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" {
			t.Fatalf("unexpected route %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":"um_1","alias":"studio"},{"id":"um_2","alias":"Studio"}],"pagination":{"limit":200,"offset":0,"total":2,"next_offset":null}}}`))
	}))
	defer server.Close()

	_, _, _, err := selectTerminalSession(context.Background(), api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client()), "studio", "", "named")
	if !errors.Is(err, resolver.ErrMachineAmbiguous) {
		t.Fatalf("err = %v, want machine ambiguity", err)
	}
}

func TestSelectTerminalSessionCreatesFreshSessionByDefault(t *testing.T) {
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "studio", "state": "ready"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			created = true
			writeAPIData(t, w, map[string]any{"id": "pts_1", "name": "quiet-harbor", "state": "open", "created_at": time.Now(), "updated_at": time.Now()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, _, _, err := selectTerminalSession(context.Background(), api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client()), "studio", "", "")
	if err != nil || got.ID != "pts_1" || got.Name != "quiet-harbor" || !created {
		t.Fatalf("fresh session = %+v, %v; created=%t", got, err, created)
	}
}

func TestTerminalArgsAcceptsOnlyOptionalNewKeyword(t *testing.T) {
	validate := terminalArgs(0)
	for _, values := range [][]string{nil, {"studio"}, {"studio", "new"}} {
		if err := validate(nil, values); err != nil {
			t.Fatalf("values %q rejected: %v", values, err)
		}
	}
	if err := validate(nil, []string{"studio", "attach"}); err == nil {
		t.Fatal("unexpected second keyword accepted")
	}
}

func TestConnectWithServerURLUsesBackendResolver(t *testing.T) {
	dir := t.TempDir()
	stateRoot := filepath.Join(dir, "runtime")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
	identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	inboxPath := filepath.Join(dir, "Paperboat Inbox")
	if err := os.MkdirAll(inboxPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.SaveRegistration(identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_source", EnvironmentID: "env_source", PublicKeyID: identityStore.Current().ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(identityStore.Current().Public()), InboxPath: inboxPath, InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	var sawMachines bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/machines" {
			sawMachines = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"items":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	writeTestProfile(t, dir, configPath, server.URL)

	err = newApp().Run([]string{"pb", "--config", configPath, "--server", server.URL, "demo"})
	if err == nil {
		t.Fatal("expected machine lookup error")
	}
	if !sawMachines {
		t.Fatal("expected backend machine list request")
	}
	if !strings.Contains(err.Error(), "machine not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestHelpCommandDoesNotCallBackend(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output
	if err := app.Run([]string{"pb", "help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage:") || !strings.Contains(output.String(), "Available Commands:") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestVersionFlags(t *testing.T) {
	previous := buildinfo.Version
	buildinfo.Version = "2026.07.25.0"
	t.Cleanup(func() { buildinfo.Version = previous })
	for _, flag := range []string{"--version", "-v"} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{flag}, &stdout, &stderr); code != 0 || stdout.String() != versionDisplay("2026.07.25.0") || stderr.Len() != 0 {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", flag, code, stdout.String(), stderr.String())
		}
	}
}

func TestVersionDisplayIncludesBrandAndVersion(t *testing.T) {
	display := versionDisplay("2026.08.02.0")
	if !strings.Contains(display, "Paperboat") || !strings.Contains(display, "Version 2026.08.02.0") {
		t.Fatalf("version display = %q", display)
	}
}

func TestBrandDisplayAlignsMetadataByTerminalCellWidth(t *testing.T) {
	display := brandDisplay("2026.08.02.0", "person@example.com")
	details := []string{"Paperboat", "Version 2026.08.02.0", "person@example.com"}
	lines := strings.Split(display, "\n")
	if len(lines) != len(details) {
		t.Fatalf("brand lines = %q", lines)
	}
	column := -1
	for index, detail := range details {
		position := strings.Index(lines[index], detail)
		if position < 0 {
			t.Fatalf("line %d missing %q: %q", index, detail, lines[index])
		}
		cell := ansi.StringWidth(lines[index][:position])
		if column < 0 {
			column = cell
		} else if cell != column {
			t.Fatalf("line %d metadata column = %d, want %d: %q", index, cell, column, lines[index])
		}
	}
}

func TestMachineTransportSnapshotRetainsAutoMarkerForMixedPaths(t *testing.T) {
	bar := statusbar.New(statusbar.Options{Mode: statusbar.ModeOff, Layout: statusbar.Layout{Right: []string{"connection"}}})
	bar.SetConnection("connected")
	applyMachineTransportPath(localapi.Snapshot{Machines: []localapi.MachineStatus{{ID: "machine_1", SelectedPath: "relay"}}}, "machine_1", bar)
	applyMachineTransportPath(localapi.Snapshot{Machines: []localapi.MachineStatus{{ID: "machine_1", SelectedPath: "mixed", ActiveConsumers: 2, TransportConsumers: []localapi.TransportConsumer{{Path: "direct", ActiveConsumers: 1}, {Path: "relay", ActiveConsumers: 1, RelayRegion: "bom"}}}}}, "machine_1", bar)
	if line := bar.Render(40); !strings.Contains(line, "connected  q") {
		t.Fatalf("mixed aggregate erased automatic marker: %q", line)
	}
}

func TestCanonicalCommandsAreDiscoverable(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{{"login"}, {"auth", "login"}, {"auth", "switch"}, {"logout"}, {"pair"}, {"session", "attach"}, {"session", "list"}, {"machine", "add"}, {"machine", "list"}, {"machine", "rename"}, {"machine", "revoke"}, {"machine", "availability"}, {"preview", "list"}, {"preview", "stop"}} {
		command, remaining, err := root.Find(path)
		if err != nil || len(remaining) != 0 || command == root {
			t.Fatalf("command %q not discoverable: command=%v remaining=%q err=%v", path, command, remaining, err)
		}
	}
	for _, removed := range []string{"create", "projects"} {
		command, remaining, err := root.Find([]string{removed})
		if err != nil || command != root || len(remaining) != 1 {
			t.Fatalf("removed command %q is still discoverable: command=%v remaining=%q err=%v", removed, command, remaining, err)
		}
	}
}

func TestMachineSessionActivityUsesNewestAvailableTimestamp(t *testing.T) {
	created := time.Unix(10, 0)
	updated := time.Unix(20, 0)
	active := time.Unix(30, 0)
	if got := (machineSession{session: api.TerminalSession{CreatedAt: created, UpdatedAt: updated, LastActiveAt: &active}}).activity(); !got.Equal(active) {
		t.Fatalf("activity=%v want=%v", got, active)
	}
	if got := (machineSession{session: api.TerminalSession{CreatedAt: created, UpdatedAt: updated}}).activity(); !got.Equal(updated) {
		t.Fatalf("activity=%v want=%v", got, updated)
	}
	if got := (machineSession{session: api.TerminalSession{CreatedAt: created}}).activity(); !got.Equal(created) {
		t.Fatalf("activity=%v want=%v", got, created)
	}
}

func TestFavoritesSortFirstWithoutReorderingPeers(t *testing.T) {
	values := []struct {
		id       string
		favorite bool
	}{{"first", false}, {"star-a", true}, {"second", false}, {"star-b", true}}
	slices.SortStableFunc(values, func(a, b struct {
		id       string
		favorite bool
	}) int {
		return compareFavorites(a.favorite, b.favorite)
	})
	got := make([]string, 0, len(values))
	for _, value := range values {
		got = append(got, value.id)
	}
	if strings.Join(got, ",") != "star-a,star-b,first,second" {
		t.Fatalf("order=%v", got)
	}
}

func TestMaskEmailHidesAddressUntilExplicitReveal(t *testing.T) {
	if got := maskEmail("person@example.com"); got != "██████@███████.███" {
		t.Fatalf("masked email=%q", got)
	}
	if got := maskEmail("Not signed in"); got != "Not signed in" {
		t.Fatalf("non-email=%q", got)
	}
}

func TestMachineAvailabilityRequiresConfirmationAndReturnsAppliedJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "studio", "availability": map[string]any{"schema": "paperboat.availability-policy/v1", "desired_mode": "allow_sleep", "desired_version": 3, "observed_version": 3, "status": "applied"}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case "PUT /v1/machines/um_1/availability-policy":
			if r.Header.Get("Idempotency-Key") == "" {
				t.Fatal("missing idempotency key")
			}
			writeAPIData(t, w, map[string]any{"schema": "paperboat.availability-policy/v1", "desired_mode": "keep_awake", "desired_version": 4, "observed_mode": "keep_awake", "observed_version": 4, "status": "applied", "observed_at": "2026-07-26T12:00:00Z", "host_service_version": "test", "host_service_scope": "system"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)
	var stdout, stderr bytes.Buffer
	if code := runWithConfirmation(t, []string{"--config", configPath, "machine", "availability", "studio", "--mode", "keep-awake", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var output struct {
		Version string `json:"version"`
		Outcome string `json:"outcome"`
		Retry   string `json:"retry"`
		Machine struct {
			ID    string `json:"id"`
			Alias string `json:"alias"`
		} `json:"machine"`
		Availability api.AvailabilityPolicy `json:"availability"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output.Version != "1" || output.Outcome != "applied" || output.Retry != "automatic" || output.Machine.ID != "um_1" || output.Machine.Alias != "studio" || output.Availability.DesiredVersion != 4 {
		t.Fatalf("output=%q decoded=%+v err=%v", stdout.String(), output, err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q, want empty", stderr.String())
	}
}

func TestMachineRevokeRequiresConfirmationBeforeBackend(t *testing.T) {
	root := newRootCommand()
	command, _, err := root.Find([]string{"machine", "revoke"})
	if err != nil || command.Flags().Lookup("confirm") == nil || command.Flags().Lookup("yes") != nil {
		t.Fatalf("confirmation flags: command=%v err=%v", command, err)
	}
}

func TestSessionCloseRequiresConfirmationBeforeBackend(t *testing.T) {
	for _, tc := range []struct {
		name, operation, state string
		status                 int
	}{
		{"pending", "pending", "closing", http.StatusAccepted},
		{"applied", "applied", "closed", http.StatusOK},
		{"missing receipt", "", "", http.StatusOK},
		{"backend rejection", "", "", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			closes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
					writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
					writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_1", "name": "task", "state": "running"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/um_1/terminal-sessions/ses_1/close":
					closes++
					w.WriteHeader(tc.status)
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"operation_state": tc.operation}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			writeTestProfile(t, dir, configPath, srv.URL)
			args := []string{"--config", configPath, "session", "close", "demo", "ses_1", "--json"}
			var preview, output, errorsOutput bytes.Buffer
			if code := run(context.Background(), args, &preview, &errorsOutput); code != 2 || closes != 0 {
				t.Fatalf("preview code=%d mutations=%d", code, closes)
			}
			token := previewConfirmationCode(t, preview.String())
			errorsOutput.Reset()
			code := run(context.Background(), append(args, "--confirm", token), &output, &errorsOutput)
			if closes != 1 {
				t.Fatalf("close requests=%d", closes)
			}
			if tc.state == "" {
				if code == 0 || strings.Contains(output.String(), `"state":"closed"`) {
					t.Fatalf("failed close reported success: code=%d output=%s", code, output.String())
				}
				return
			}
			var result struct {
				State          string `json:"state"`
				OperationState string `json:"operation_state"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil || code != 0 || result.State != tc.state || result.OperationState != tc.operation {
				t.Fatalf("code=%d result=%+v err=%v", code, result, err)
			}
		})
	}
}

func TestSessionCloseAllRequiresConfirmationAndRejectsSessionArgument(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	mutated := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_1", "name": "default", "state": "open"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		default:
			mutated = true
			t.Fatalf("unexpected request before confirmation: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "session", "close", "demo", "--all"}, &output, &output); code != 2 || mutated || !strings.Contains(output.String(), "Environment: demo (um_1)") || !strings.Contains(output.String(), "Open sessions to close: 1") || !strings.Contains(output.String(), "To confirm within 5 minutes") {
		t.Fatalf("code=%d mutated=%t output=%q", code, mutated, output.String())
	}

	root := newRootCommand()
	root.SetArgs([]string{"session", "close", "um_1", "shell-2", "--all", "--confirm", "AAAAAA"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "usage: pb session close") {
		t.Fatalf("err=%v, want mutually exclusive usage error", err)
	}
}

func previewConfirmationCode(t *testing.T, output string) string {
	t.Helper()
	if strings.HasPrefix(output, "{") {
		var result struct {
			Data struct {
				Token string `json:"confirmation_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatal(err)
		}
		if validConfirmationCode(result.Data.Token) {
			return result.Data.Token
		}
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		t.Fatal("missing confirmation command")
	}
	token := fields[len(fields)-1]
	if !validConfirmationCode(token) {
		t.Fatalf("invalid confirmation token in %q", output)
	}
	return token
}

func runWithConfirmation(t *testing.T, args []string, output, errorsOutput *bytes.Buffer) int {
	t.Helper()
	var preview, previewErrors bytes.Buffer
	if code := run(context.Background(), args, &preview, &previewErrors); code != 2 {
		t.Fatalf("preview code=%d stdout=%q stderr=%q", code, preview.String(), previewErrors.String())
	}
	token := previewConfirmationCode(t, preview.String())
	return run(context.Background(), append(append([]string(nil), args...), "--confirm", token), output, errorsOutput)
}

func TestSessionCloseAllClosesEveryOpenSession(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	var closed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_1", "name": "default", "state": "open"}, {"id": "ses_2", "name": "api", "state": "open"}, {"id": "ses_3", "name": "old", "state": "closed"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 3, "next_offset": nil}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			closed = append(closed, r.URL.Path)
			operation := "applied"
			if strings.Contains(r.URL.Path, "/ses_2/") {
				operation = "pending"
			}
			writeAPIData(t, w, map[string]any{"operation_state": operation})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "session", "close", "demo", "--all"}, &output, &output); code != 2 {
		t.Fatalf("preview code=%d output=%q", code, output.String())
	}
	token := previewConfirmationCode(t, output.String())
	output.Reset()
	if code := run(context.Background(), []string{"--config", configPath, "session", "close", "demo", "--all", "--confirm", token}, &output, &output); code != 0 {
		t.Fatalf("code=%d output=%q", code, output.String())
	}
	if len(closed) != 2 || !strings.Contains(closed[0], "ses_1") || !strings.Contains(closed[1], "ses_2") {
		t.Fatalf("closed=%v", closed)
	}
	if !strings.Contains(output.String(), "Closed 1 sessions in demo.") || !strings.Contains(output.String(), "1 close requests are pending host confirmation.") {
		t.Fatalf("output=%q", output.String())
	}
}

func TestSessionsCloseAllClosesEveryOpenSessionAndRetainsHistory(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	var closed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_1", "name": "default", "state": "open"}, {"id": "ses_2", "name": "api", "state": "open"}, {"id": "ses_3", "name": "old", "state": "closed"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 3, "next_offset": nil}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			closed = append(closed, r.URL.Path)
			writeAPIData(t, w, map[string]any{"operation_state": "applied"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "session", "close", "demo", "--all"}, &output, &output); code != 2 {
		t.Fatalf("preview code=%d output=%q", code, output.String())
	}
	token := previewConfirmationCode(t, output.String())
	output.Reset()
	if code := run(context.Background(), []string{"--config", configPath, "session", "close", "demo", "--all", "--confirm", token}, &output, &output); code != 0 {
		t.Fatalf("code=%d output=%q", code, output.String())
	}
	if len(closed) != 2 || !strings.Contains(output.String(), "Open sessions to close: 2") || !strings.Contains(output.String(), "Recent output was deleted") {
		t.Fatalf("closed=%v output=%q", closed, output.String())
	}
}

func TestSessionDeleteAllRequiresConfirmationAndRejectsSessionArgument(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	mutated := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_1", "name": "default", "state": "open", "is_default": true}, {"id": "ses_2", "name": "work", "state": "closed"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 2, "next_offset": nil}})
		default:
			mutated = true
			t.Fatalf("unexpected request before confirmation: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "session", "delete", "demo", "--all"}, &output, &output); code != 2 || mutated || !strings.Contains(output.String(), "Closed non-default sessions to delete: 1") || !strings.Contains(output.String(), "To confirm within 5 minutes") {
		t.Fatalf("code=%d mutated=%t output=%q", code, mutated, output.String())
	}

	root := newRootCommand()
	root.SetArgs([]string{"session", "delete", "um_1", "shell-2", "--all", "--confirm", "AAAAAA"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "usage: pb session delete") {
		t.Fatalf("err=%v, want mutually exclusive usage error", err)
	}
}

func TestSessionDeleteAllDeletesOnlyClosedNonDefaultSessions(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_1", "name": "default", "state": "open", "is_default": true}, {"id": "ses_2", "name": "work", "state": "open"}, {"id": "ses_3", "name": "old", "state": "closed"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 3, "next_offset": nil}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/machines/um_1/terminal-sessions/"):
			deleted = append(deleted, r.URL.Path)
			writeAPIData(t, w, map[string]any{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "session", "delete", "demo", "--all"}, &output, &output); code != 2 {
		t.Fatalf("preview code=%d output=%q", code, output.String())
	}
	token := previewConfirmationCode(t, output.String())
	output.Reset()
	if code := run(context.Background(), []string{"--config", configPath, "session", "delete", "demo", "--all", "--confirm", token, "--json"}, &output, &output); code != 0 {
		t.Fatalf("code=%d output=%q", code, output.String())
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/ses_3") || !strings.Contains(output.String(), `"deleted":1`) {
		t.Fatalf("deleted=%v output=%q", deleted, output.String())
	}
}

func TestSessionDeleteOpenSessionRequiresClose(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "ses_2", "name": "work", "state": "open"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/machines/um_1/terminal-sessions/ses_2":
			deleted = true
			writeAPIData(t, w, map[string]any{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "session", "delete", "demo", "work", "--json"}, &output, &output); code != 1 || deleted || !strings.Contains(output.String(), "close the terminal session before deleting") {
		t.Fatalf("code=%d deleted=%t output=%q", code, deleted, output.String())
	}
}

func TestSessionListAcceptsDefaultEnvironment(t *testing.T) {
	root := newRootCommand()
	command, remaining, err := root.Find([]string{"session", "list"})
	if err != nil || command == root || len(remaining) != 0 {
		t.Fatalf("session list lookup command=%v remaining=%q err=%v", command, remaining, err)
	}
	if err := command.Args(command, nil); err != nil {
		t.Fatalf("session list rejected omitted environment: %v", err)
	}
}
