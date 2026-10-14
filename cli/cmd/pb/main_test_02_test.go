package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	bugreportpkg "github.com/pinksaucepasta/paperboat/internal/bugreport"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestInboxSpecTreeRegistersJSONForPathSetAndReset(t *testing.T) {
	tree := specTree(inboxCommand(), "inbox")
	for _, name := range []string{"path", "set", "reset"} {
		entry, _, err := tree.Find([]string{name})
		if err != nil {
			t.Fatalf("inbox %s: %v", name, err)
		}
		flag := entry.Flags().Lookup("json")
		if flag == nil || flag.Value.Type() != "bool" {
			t.Fatalf("inbox %s --json=%v", name, flag)
		}
	}
}

func TestInboxSpecTreeEnforcesPositionalArguments(t *testing.T) {
	const directory = "fixture-inbox-directory"
	called := false
	tree := specTreeActionTestCommand(t, inboxCommand(), "set", func(ctx *command.Context) error {
		called = true
		if ctx.Args().Len() != 1 || ctx.Args().First() != directory {
			t.Fatalf("set args len=%d first=%q", ctx.Args().Len(), ctx.Args().First())
		}
		return nil
	})
	tree.SetOut(io.Discard)
	tree.SetErr(io.Discard)
	tree.SetArgs([]string{"set", directory})
	if err := tree.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("set one directory: %v", err)
	}
	if !called {
		t.Fatal("set action was not called")
	}

	for _, test := range []struct {
		name  string
		child string
		args  []string
	}{
		{name: "set zero", child: "set", args: []string{"set"}},
		{name: "set two", child: "set", args: []string{"set", directory, "extra"}},
		{name: "path one", child: "path", args: []string{"path", directory}},
		{name: "reset one", child: "reset", args: []string{"reset", directory}},
	} {
		t.Run(test.name, func(t *testing.T) {
			actionCalled := false
			commandTree := specTreeActionTestCommand(t, inboxCommand(), test.child, func(*command.Context) error {
				actionCalled = true
				return nil
			})
			commandTree.SetOut(io.Discard)
			commandTree.SetErr(io.Discard)
			commandTree.SetArgs(test.args)
			if err := commandTree.ExecuteContext(context.Background()); err == nil {
				t.Fatal("invalid arguments succeeded")
			}
			if actionCalled {
				t.Fatal("action was called for invalid arguments")
			}
		})
	}
}

func TestInboxPathJSONExecutesWithDisposableIdentity(t *testing.T) {
	rootPath := commandRuntimeTestRoot(t)
	home := filepath.Join(rootPath, "home")
	runtimeRoot := filepath.Join(rootPath, "runtime")
	stateRoot := filepath.Join(rootPath, "identity")
	inboxPath := filepath.Join(home, "Paperboat Inbox")
	for _, directory := range []string{home, runtimeRoot, inboxPath} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(rootPath, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(rootPath, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
	configPath := filepath.Join(rootPath, "config.json")
	const serverURL = "https://api.paperboat.test"
	if err := os.WriteFile(configPath, []byte(`{"server_url":"`+serverURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	key := identityStore.Current()
	if err := identityStore.SaveRegistration(identity.Registration{
		ServerURL: serverURL, MachineID: "machine_inbox_fixture", EnvironmentID: "environment_inbox_fixture",
		PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: inboxPath,
		InstallationGeneration: 1, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config", configPath, "inbox", "path", "--json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v stderr=%q", err, stderr.String())
	}
	var payload struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Data          struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if payload.SchemaVersion != "1.0" || !payload.OK || payload.Data.Path != inboxPath || stderr.Len() != 0 {
		t.Fatalf("payload=%#v stderr=%q", payload, stderr.String())
	}
}

func TestShellCompletionIsBoundedSilentAndResourceSpecific(t *testing.T) {
	rootPath := commandRuntimeTestRoot(t)
	t.Setenv("HOME", rootPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(rootPath, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(rootPath, "runtime"))
	t.Setenv("TMPDIR", filepath.Join(rootPath, "runtime"))
	if err := os.MkdirAll(filepath.Join(rootPath, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	for _, path := range [][]string{{"ping"}, {"wait"}, {"preview", "stop"}, {"session", "attach"}, {"session", "delete"}} {
		command, _, err := root.Find(path)
		if err != nil || command.ValidArgsFunction == nil {
			t.Fatalf("completion hook missing for %v: %v", path, err)
		}
	}
	ping, _, _ := root.Find([]string{"ping"})
	started := time.Now()
	values, directive := ping.ValidArgsFunction(ping, nil, "ma")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing-daemon completion took %s", elapsed)
	}
	if len(values) != 0 || directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("values=%v directive=%v", values, directive)
	}

	items := []api.TerminalSession{{ID: "session_02", Name: "worker", State: "running"}, {ID: "session_01", Name: "web", State: "stopped"}}
	got := sessionCompletionValues(items, "w")
	want := []string{"web\tstopped", "worker\trunning"}
	if !slices.Equal(got, want) {
		t.Fatalf("session completions=%v, want %v", got, want)
	}
	for _, value := range got {
		for _, forbidden := range []string{"token", "endpoint", "address", "fingerprint"} {
			if strings.Contains(strings.ToLower(value), forbidden) {
				t.Fatalf("completion exposed forbidden category %q: %q", forbidden, value)
			}
		}
	}
}

func TestPerformUninstallCleanupRunsEveryStepAndAggregatesFailures(t *testing.T) {
	var calls []string
	failure := errors.New("control service unavailable")
	err := performUninstallCleanup([]uninstallCleanupStep{
		{name: "previews", run: func() error { calls = append(calls, "previews"); return failure }},
		{name: "daemon", run: func() error { calls = append(calls, "daemon"); return nil }},
		{name: "runtime", run: func() error { calls = append(calls, "runtime"); return errors.New("service removal failed") }},
		{name: "user state", run: func() error { calls = append(calls, "user-state"); return nil }},
		{name: "product", run: func() error { calls = append(calls, "product"); return nil }},
	})
	if got, want := strings.Join(calls, ","), "previews,daemon,runtime,user-state,product"; got != want {
		t.Fatalf("cleanup calls = %q, want %q", got, want)
	}
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "previews:") || !strings.Contains(err.Error(), "runtime:") {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestRemovePathPreservingKeepsNestedInboxAndRemovesOtherState(t *testing.T) {
	root := t.TempDir()
	inboxPath := filepath.Join(root, "state", "Paperboat Inbox")
	if err := os.MkdirAll(inboxPath, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(inboxPath, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("user content"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "state", "runtime.json"), filepath.Join(root, "cache", "artifact.bin")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("remove"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removePathPreserving(root, []string{inboxPath}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "user content" {
		t.Fatalf("Inbox sentinel content=%q error=%v", content, err)
	}
	for _, path := range []string{filepath.Join(root, "state", "runtime.json"), filepath.Join(root, "cache")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-Inbox state remains at %s: %v", path, err)
		}
	}
}

func TestRemovePathPreservingNeverRemovesCleanupRootInsideInbox(t *testing.T) {
	inboxPath := t.TempDir()
	stateRoot := filepath.Join(inboxPath, "custom-state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stateRoot, "user-named-file.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePathPreserving(stateRoot, []string{inboxPath}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep" {
		t.Fatalf("Inbox subtree changed: content=%q error=%v", content, err)
	}
}

func TestRemovePathPreservingRemovesDanglingPaperboatSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "pb")
	if err := os.Symlink(filepath.Join(root, "missing-paperboat-binary"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := removePathPreserving(link, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling CLI symlink remains: %v", err)
	}
}

func TestRemovePathPreservingRefusesSymlinkAncestorOfInbox(t *testing.T) {
	external := t.TempDir()
	if err := os.Mkdir(filepath.Join(external, "Inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(external, "user-data.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "paperboat-state")
	if err := os.Symlink(external, root); err != nil {
		t.Fatal(err)
	}
	err := removePathPreserving(root, []string{filepath.Join(root, "Inbox")})
	if err == nil || !strings.Contains(err.Error(), "unsafe Paperboat cleanup path") {
		t.Fatalf("symlink ancestor error = %v", err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep" {
		t.Fatalf("external user data content=%q error=%v", content, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func writeAPIData(t *testing.T, writer http.ResponseWriter, data any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{"data": data}); err != nil {
		t.Fatal(err)
	}
}

func TestUserFacingErrorSanitizesInfrastructureFailures(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		want   string
		forbid []string
	}{
		{
			name:   "network details",
			err:    fmt.Errorf("list machines: call GET /v1/machines: %w", &net.DNSError{Err: "no such host", Name: "api.secret.example"}),
			want:   "Paperboat is unreachable.",
			forbid: []string{"GET", "/v1/machines", "api.secret.example"},
		},
		{
			name: "local filesystem failure",
			err:  &os.PathError{Op: "open", Path: `C:\\Users\\Pujan\\Paperboat\\daemon.lock`, Err: os.ErrPermission},
			want: "could not access local files",
		},
		{
			name:   "local permission failure",
			err:    fmt.Errorf("start local daemon: %w", syscall.EACCES),
			want:   "could not access a local resource",
			forbid: []string{"unreachable", "network connection"},
		},
		{
			name: "operation deadline",
			err:  fmt.Errorf("peer stream setup: %w", context.DeadlineExceeded),
			want: "operation timed out",
		},
		{
			name:   "server outage",
			err:    &api.APIError{Status: http.StatusBadGateway, RequestID: "req_123"},
			want:   "Paperboat is temporarily unavailable.",
			forbid: []string{"status 502", "paperboat-server"},
		},
		{
			name:   "terminal loss",
			err:    errors.Join(tunnel.ErrTransportLost, errors.New("Application error 0x5042 (remote): server draining")),
			want:   "could not finish the command",
			forbid: []string{"0x5042", "server draining", "transport lost"},
		},
		{
			name: "missing private transport custody",
			err:  config.ErrSecretNotFound,
			want: "not paired for private transport",
		},
		{
			name: "pairing required",
			err:  identitybootstrap.ErrPairingRequired,
			want: "needs approval from a paired machine",
		},
		{
			name: "pairing approval expired",
			err:  identitybootstrap.ErrEnrollmentExpired,
			want: "Run the enrollment command from the Paperboat dashboard",
		},
		{
			name:   "uninstall cleanup keeps safe stage",
			err:    uninstallCleanupError{err: uninstallStepFailure{name: "stop local daemon", err: &net.DNSError{Err: "no such host", Name: "api.secret.example"}}},
			want:   "Failed: stop local daemon",
			forbid: []string{"api.secret.example", "no such host", "Paperboat is unreachable"},
		},
		{
			name:   "native peer authority details",
			err:    fmt.Errorf("open machine: %w", tailnet.ErrAuthority),
			want:   "secure connection could not be established",
			forbid: []string{"authority is invalid"},
		},
		{
			name:   "native peer admission details",
			err:    fmt.Errorf("open machine: %w", tailnet.ErrAdmission),
			want:   "secure connection could not be established",
			forbid: []string{"virtual UDP", "peer and port"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := userFacingError(test.err)
			if !strings.Contains(got, test.want) {
				t.Fatalf("message = %q, want %q", got, test.want)
			}
			for _, forbidden := range test.forbid {
				if strings.Contains(got, forbidden) {
					t.Fatalf("message = %q contains %q", got, forbidden)
				}
			}
		})
	}
}

func TestRunTreatsCancellationAsUserCancellation(t *testing.T) {
	if got := userFacingError(context.Canceled); got != "Operation canceled." {
		t.Fatalf("message = %q", got)
	}
}

func TestWrappedNativeSSHExitIsAnInvocationFailureWithoutAnException(t *testing.T) {
	for _, code := range []int{7, 143, 255} {
		err := fmt.Errorf("SSH invocation: %w", managedssh.NativeExitError{Code: code, Err: errors.New("PRIVATE_PAYLOAD")})
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != code || !nativeExitFailure(err) || unexpectedCLIError(err) || operationalCLIError(err) {
			t.Fatalf("native status %d was misclassified", code)
		}
	}
	launch := managedssh.NativeLaunchError{Err: &os.PathError{Op: "fork/exec", Path: "PRIVATE_PAYLOAD", Err: syscall.ENOENT}}
	if !unexpectedCLIError(launch) || nativeExitFailure(launch) {
		t.Fatal("native launch failure was mistaken for a completed native command")
	}
}

func TestPresentedStructuredFailureRetainsCauseAndInvocationClassification(t *testing.T) {
	cause := errors.New("PRIVATE_BUNDLE_FAILURE")
	err := presentedCommandFailure{cause: &bugreportpkg.StageError{Stage: "create local bundle", Err: cause}}
	if !errors.Is(err, cause) || err.ExitCode() != 1 || !unexpectedCLIError(err) {
		t.Fatal("structured result erased the owning command failure")
	}
	fault := errorreport.ProjectFault(t.Context(), "pb", "bugreport", "command", "unexpected_cli_failure", err)
	if fault.Stage != "diagnostic_storage" || fault.Code != "command_failed" || fault.Cause != "internal" {
		t.Fatalf("structured failure lost stage/cause: %#v", fault)
	}
	if (presentedCommandFailure{cause: context.Canceled}).ExitCode() != 130 || (presentedCommandFailure{cause: errors.Join(context.Canceled, context.DeadlineExceeded)}).ExitCode() != 1 {
		t.Fatal("structured failure lost timeout/cancellation distinction")
	}
}

type failingBugreportRefresh struct{ cause error }

func (f failingBugreportRefresh) Credential() (config.Credential, error) {
	return config.Credential{AccessToken: "test-token"}, nil
}
func (f failingBugreportRefresh) Refresh() (config.Credential, error) {
	return config.Credential{}, f.cause
}

func TestBugreportUploadPreservesRefreshFailureInsteadOfReportingLogout(t *testing.T) {
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"PRIVATE_PROVIDER_DETAIL"}}`)
	}))
	defer backend.Close()
	cause := syscall.ECONNRESET
	server := &refreshingBugreportServer{serverURL: backend.URL, auth: failingBugreportRefresh{cause}, client: api.New(backend.URL, config.Credential{AccessToken: "expired-test-token"}, backend.Client())}
	_, err := server.CreateDiagnosticUploadIntent(t.Context(), "operation_test", api.DiagnosticUploadIntentRequest{Schema: api.DiagnosticUploadIntentRequestSchemaV1})
	if !errors.Is(err, cause) || errors.Is(err, api.ErrUnauthenticated) {
		t.Fatal("intent refresh failure was mistaken for logout")
	}
	_, err = server.CompleteDiagnosticUploadIntent(t.Context(), "diag_test")
	if !errors.Is(err, cause) || requests.Load() != 2 {
		t.Fatal("completion refresh failure lost its cause or retried without a credential")
	}
}

type failingCommandDiagnosticService struct{}

func (failingCommandDiagnosticService) Diagnostics(context.Context) (localapi.DiagnosticSnapshot, error) {
	return localapi.DiagnosticSnapshot{}, errors.New("PRIVATE_DIAGNOSTIC_FAILURE")
}
func (failingCommandDiagnosticService) RecordBugreportMarker(context.Context, string) error {
	return errors.New("PRIVATE_MARKER_FAILURE")
}
func (failingCommandDiagnosticService) CreateBugreport(context.Context) (diagnostics.Bundle, error) {
	return diagnostics.Bundle{}, syscall.ENOSPC
}

func TestBugreportJSONFailureAcrossLocalAPIHasOneResultAndCorrelatedCause(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	home, runtimeRoot := filepath.Join(root, "home"), filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	store, err := localapi.NewSnapshotStore(&localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: time.Now().UTC(), DaemonState: "ready", DaemonVersion: "dev", Machines: []localapi.MachineStatus{}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := commandLocalAPIServerConfig(paths.SocketPath, store)
	if err != nil {
		t.Fatal(err)
	}
	config.Diagnostics = failingCommandDiagnosticService{}
	server, err := localapi.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(serverCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("task-owned local API did not stop")
		}
	})
	waitForCommandSocket(t, paths.SocketPath)
	restore := errorreport.Install(nil)
	defer restore()
	var mu sync.Mutex
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, fault)
		mu.Unlock()
	})
	defer restoreObserver()
	reference := supportref.New()
	var stdout, stderr bytes.Buffer
	if code := runWithReporter(supportref.WithContext(t.Context(), reference), []string{"--config", filepath.Join(root, "config.json"), "bugreport", "--json"}, &stdout, &stderr, nil); code != 1 {
		t.Fatalf("command status=%d", code)
	}
	decoder := json.NewDecoder(&stdout)
	var result bugreportpkg.Result
	if err := decoder.Decode(&result); err != nil || result.Error == nil || result.Error.SupportReference != reference || result.Error.Code != "bugreport_failed" || result.Validate() != nil {
		t.Fatalf("missing structured failure/reference: result=%#v decode=%v", result, err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || stderr.Len() != 0 {
		t.Fatal("failure printed a second result or leaked interactive stderr")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(faults) != 2 || faults[0].Cause != "resource_exhausted" || faults[0].Stage != "diagnostic_storage" || faults[1].HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("local API/command lost original failure: %#v", faults)
	}
	for _, fault := range faults {
		if fault.SupportReference != reference {
			t.Fatal("local API and command diagnostic references diverged")
		}
	}
}

func TestRemoteExecCommandFormsReachExecutionInsteadOfTerminalUsage(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{
		{"--config", configPath, "Studio", "--", "/bin/echo", "hello world"},
		{"--config", configPath, "exec", "Studio", "--env", "VALUE=hello world", "--", "/bin/echo", "hello world"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code == 2 {
			t.Fatalf("args=%v treated as usage error: %s", args, stderr.String())
		}
	}
}

func TestWriteExecJSONFailure(t *testing.T) {
	var output bytes.Buffer
	if err := writeExecJSONFailure(&output, "exec-operation-1", "exec_start_uncertain", "root cause", true, true); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Version     string `json:"version"`
		Event       string `json:"event"`
		OperationID string `json:"operation_id"`
		ErrorCode   string `json:"error_code"`
		Changed     bool   `json:"changed"`
		Uncertain   bool   `json:"uncertain"`
	}
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Version != "paperboat.exec-event/v1" || event.Event != "failed" || event.OperationID != "exec-operation-1" || event.ErrorCode != "exec_start_uncertain" || !event.Changed || !event.Uncertain {
		t.Fatalf("event = %#v", event)
	}
}

func TestRemoteExecAbsolutePathUsesTargetPlatform(t *testing.T) {
	tests := []struct {
		name          string
		platform      string
		workspaceRoot string
		cwd           string
		want          bool
	}{
		{name: "Windows client to Linux root", platform: "linux", workspaceRoot: "/root", cwd: "/root", want: true},
		{name: "Windows client to macOS root", platform: "darwin", workspaceRoot: "/Users/paperboat", cwd: "/tmp", want: true},
		{name: "Unix client to Windows root", platform: "windows", workspaceRoot: `C:\workspace`, cwd: `C:\workspace`, want: true},
		{name: "platform inferred from Linux workspace", workspaceRoot: "/root", cwd: "/root/project", want: true},
		{name: "platform inferred from Windows workspace", workspaceRoot: `C:\workspace`, cwd: `D:\project`, want: true},
		{name: "Linux rejects Windows cwd", platform: "linux", workspaceRoot: "/root", cwd: `C:\workspace`},
		{name: "Windows rejects Linux cwd", platform: "windows", workspaceRoot: `C:\workspace`, cwd: "/root"},
		{name: "relative cwd rejected", platform: "linux", workspaceRoot: "/root", cwd: "project"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := remoteAbsolutePath(test.platform, test.workspaceRoot, test.cwd); got != test.want {
				t.Fatalf("remoteAbsolutePath(%q, %q, %q) = %v, want %v", test.platform, test.workspaceRoot, test.cwd, got, test.want)
			}
		})
	}
}

type execInputTestConn struct {
	mu         sync.Mutex
	writes     [][]byte
	writeErr   error
	closeWrite bool
}

func (c *execInputTestConn) Read([]byte) (int, error)        { return 0, io.EOF }
func (c *execInputTestConn) Close() error                    { return nil }
func (c *execInputTestConn) Detach() error                   { return nil }
func (c *execInputTestConn) Resize(uint16, uint16) error     { return nil }
func (c *execInputTestConn) Signal(string) error             { return nil }
func (c *execInputTestConn) Cancel() error                   { return nil }
func (c *execInputTestConn) Wait() (int, error)              { return 0, nil }
func (c *execInputTestConn) Events() <-chan tunnel.ExecEvent { return make(chan tunnel.ExecEvent) }
func (c *execInputTestConn) Write(value []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, append([]byte(nil), value...))
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(value), nil
}
func (c *execInputTestConn) CloseWrite() error {
	c.mu.Lock()
	c.closeWrite = true
	c.mu.Unlock()
	return nil
}

type sshProxyTestConn struct {
	net.Conn
	closeWrite func() error
}

func (c *sshProxyTestConn) CloseWrite() error { return c.closeWrite() }

type sshProxyCopyFailureConn struct {
	err    error
	failed chan struct{}
	once   sync.Once
}

func (c *sshProxyCopyFailureConn) Read([]byte) (int, error) { <-c.failed; return 0, io.EOF }
func (c *sshProxyCopyFailureConn) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.failed) })
	return 0, c.err
}
func (*sshProxyCopyFailureConn) Close() error      { return nil }
func (*sshProxyCopyFailureConn) CloseWrite() error { return net.ErrClosed }

type sshProxyTrackedReader struct {
	io.Reader
	done chan struct{}
	once sync.Once
}

func (r *sshProxyTrackedReader) Read(value []byte) (int, error) {
	count, err := r.Reader.Read(value)
	if err != nil {
		r.once.Do(func() { close(r.done) })
	}
	return count, err
}

func TestCopySSHProxyRemoteEOFTerminatesWithOpenInput(t *testing.T) {
	proxy, remote := net.Pipe()
	inputReader, inputWriter := io.Pipe()
	defer inputReader.Close()
	connection := &sshProxyTestConn{Conn: proxy, closeWrite: func() error { return nil }}
	trackedInput := &sshProxyTrackedReader{Reader: inputReader, done: make(chan struct{})}
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- copySSHProxy(connection, connection, trackedInput, &output) }()
	const response = "command-output-before-close"
	if _, err := remote.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}
	_ = remote.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if output.String() != response {
			t.Fatalf("output=%q, want %q", output.String(), response)
		}
	case <-time.After(time.Second):
		t.Fatal("SSH proxy waited for input after remote EOF")
	}
	_ = inputWriter.Close()
	select {
	case <-trackedInput.done:
	case <-time.After(time.Second):
		t.Fatal("SSH proxy input copier leaked after input close")
	}
}

func TestCopySSHProxyInputEOFHalfClosesAndDrainsOutput(t *testing.T) {
	proxy, remote := net.Pipe()
	halfClosed := make(chan struct{})
	connection := &sshProxyTestConn{Conn: proxy, closeWrite: func() error {
		close(halfClosed)
		return nil
	}}
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- copySSHProxy(connection, connection, strings.NewReader("request"), &output) }()
	request := make([]byte, len("request"))
	if _, err := io.ReadFull(remote, request); err != nil || string(request) != "request" {
		t.Fatalf("request=%q err=%v", request, err)
	}
	select {
	case <-halfClosed:
	case <-time.After(time.Second):
		t.Fatal("input EOF did not half-close SSH stream")
	}
	if _, err := remote.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	_ = remote.Close()
	if err := <-done; err != nil || output.String() != "response" {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
}

func TestCopySSHProxyRemoteEOFIgnoresConcurrentClosedHalfClose(t *testing.T) {
	for _, closeErr := range []error{net.ErrClosed, io.ErrClosedPipe, &net.OpError{Op: "shutdown", Net: "unix", Err: syscall.ENOTCONN}} {
		for range 100 {
			proxy, remote := net.Pipe()
			connection := &sshProxyTestConn{Conn: proxy, closeWrite: func() error { return closeErr }}
			done := make(chan error, 1)
			go func() { done <- copySSHProxy(connection, connection, strings.NewReader("request"), io.Discard) }()
			request := make([]byte, len("request"))
			if _, err := io.ReadFull(remote, request); err != nil {
				t.Fatal(err)
			}
			_ = remote.Close()
			if err := <-done; err != nil {
				t.Fatalf("clean remote EOF returned half-close error: %v", err)
			}
		}
	}
}

func TestCopySSHProxyRemoteEOFPreservesInputCopyFailure(t *testing.T) {
	connection := &sshProxyCopyFailureConn{err: tunnel.ErrTransportLost, failed: make(chan struct{})}
	err := copySSHProxy(connection, connection, strings.NewReader("request"), io.Discard)
	if !errors.Is(err, tunnel.ErrTransportLost) {
		t.Fatalf("error=%v, want transport loss", err)
	}
}

type execChunkReader struct {
	chunks [][]byte
	index  int
}
