package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestPeerCommandsWireBoundedLocalDaemonReadiness(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	packages, err := parser.ParseDir(token.NewFileSet(), filepath.Dir(testFile), func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"runSendCommand":   "requireLocalDaemonService",
		"actionPing":       "requireLocalDaemonService",
		"actionRemoteExec": "requireLocalDaemonService",
	}
	found := make(map[string]int, len(want))
	for _, parsed := range packages["main"].Files {
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			callee, tracked := want[function.Name.Name]
			if !tracked {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				identifier, ok := call.Fun.(*ast.Ident)
				if ok && identifier.Name == callee {
					found[function.Name.Name]++
				}
				return true
			})
		}
	}
	for function, callee := range want {
		if found[function] != 1 {
			t.Fatalf("%s calls %s %d times, want 1", function, callee, found[function])
		}
	}
}

// F5-07: with a ready daemon the SSH target and alias domain come from its snapshot; Paperboat is asked nothing.
func TestResolveSSHCommandTargetWarmAsksTheServerNothing(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	home := filepath.Join(root, "home")
	runtimeRoot := filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "identity"))

	var machineListCalls, sshTargetCalls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			atomic.AddInt32(&machineListCalls, 1)
			writeAPIData(t, w, map[string]any{"items": []map[string]any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/mch_1/ssh-target":
			atomic.AddInt32(&sshTargetCalls, 1)
			if r.URL.Query().Get("machine_generation") != "4" {
				t.Errorf("machine_generation = %q", r.URL.Query().Get("machine_generation"))
			}
			writeAPIData(t, w, map[string]any{"type": "machine_target", "version": 1, "machine_id": "mch_1", "machine_generation": 4, "os_user": "root", "port": 22, "reconciliation_version": 1})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, backend.URL)

	identityRoot := filepath.Join(root, "identity")
	identityStore, err := identity.Open(identity.Config{StateRoot: identityRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := identityStore.SaveRegistration(identity.Registration{
		ServerURL:              backend.URL,
		MachineID:              "mch_source",
		EnvironmentID:          "env_source",
		PublicKeyID:            identityStore.Current().ID,
		PublicIdentityKey:      "test-public-key",
		InboxPath:              filepath.Join(home, "inbox"),
		InstallationGeneration: 1,
		UpdatedAt:              time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "dev",
		Machines: []localapi.MachineStatus{{ID: "mch_1", EnvironmentID: "env_1", WorkspaceRoot: "/root", Alias: "machine-ready", Eligible: true, RuntimeState: "ready", Generation: 4, SelectedPath: "none", TransferReadiness: "unavailable", PreviewReadiness: "unavailable", SSHReadiness: "ready", SSHUser: "root", SSHPort: 22, SSHHostKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtest"}, NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown"}},
		SSHDomain: "example.test",
	}
	store, err := localapi.NewSnapshotStore(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig, err := commandLocalAPIServerConfig(paths.SocketPath, store)
	if err != nil {
		t.Fatal(err)
	}
	server, err := localapi.NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	waitForCommandSocket(t, paths.SocketPath)

	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.String("config", configPath, "")
	set.String("server", "", "")
	commandContext := command.NewContext(set, strings.NewReader(""))

	client, machine, target, err := resolveSSHCommandTargetWarm(commandContext, "machine-ready")
	if err != nil {
		t.Fatalf("warm resolve: %v", err)
	}
	if client == nil || machine.ID != "mch_1" || machine.Alias != "machine-ready" || target.Port != 22 || target.OSUser != "root" || target.MachineGeneration != 4 || len(machine.SSHHostKeys) != 1 {
		t.Fatalf("client=%v machine=%+v target=%+v", client != nil, machine, target)
	}
	if domain, err := configuredManagedSSHDomain(commandContext); err != nil || domain != "example.test" {
		t.Fatalf("domain=%q err=%v", domain, err)
	}
	if lists, targets := atomic.LoadInt32(&machineListCalls), atomic.LoadInt32(&sshTargetCalls); lists != 0 || targets != 0 {
		t.Fatalf("warm path asked the server: %d machine lists, %d ssh-target", lists, targets)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server err=%v", err)
	}
}

func TestSelectTerminalSessionPrefersWarmMachineSnapshot(t *testing.T) {
	var catalogCalls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/machines":
			atomic.AddInt32(&catalogCalls, 1)
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/mch_1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"id": "umts_1", "name": "quiet-harbor", "state": "open", "created_at": time.Now(), "updated_at": time.Now()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "dev",
		Machines: []localapi.MachineStatus{{ID: "mch_1", EnvironmentID: "env_1", WorkspaceRoot: "/root", Alias: "machine-ready", Eligible: true, RuntimeState: "ready", Generation: 4, SelectedPath: "none", TransferReadiness: "unavailable", PreviewReadiness: "unavailable", SSHReadiness: "unavailable", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown"}},
	}
	previous := loadWarmMachineSnapshot
	loadWarmMachineSnapshot = func(context.Context) (localapi.Snapshot, error) { return snapshot, nil }
	t.Cleanup(func() { loadWarmMachineSnapshot = previous })

	session, target, machine, err := selectTerminalSession(context.Background(), api.New(backend.URL, config.Credential{AccessToken: "token"}, backend.Client()), "machine-ready", "", "")
	if err != nil {
		t.Fatalf("selectTerminalSession: %v", err)
	}
	if session.ID != "umts_1" || target.kind != environmentUserMachine || target.id != "mch_1" || machine.ID != "mch_1" {
		t.Fatalf("session=%+v target=%+v machine=%+v", session, target, machine)
	}
	if got := atomic.LoadInt32(&catalogCalls); got != 0 {
		t.Fatalf("warm machine resolution consulted the catalog %d times", got)
	}
}

func TestResolveSSHCommandTargetWarmFallsBackWithoutDaemon(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	home := filepath.Join(root, "home")
	runtimeRoot := filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "identity"))

	var machineListCalls, sshTargetCalls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			atomic.AddInt32(&machineListCalls, 1)
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "mch_1", "alias": "machine-ready", "state": "online", "online": true, "installation_generation": 4, "environment_id": "env_1", "workspace_root": "/root", "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/mch_1/ssh-target":
			atomic.AddInt32(&sshTargetCalls, 1)
			writeAPIData(t, w, map[string]any{"type": "machine_target", "version": 1, "machine_id": "mch_1", "machine_generation": 4, "os_user": "root", "port": 2222, "reconciliation_version": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, backend.URL)
	identityRoot := filepath.Join(root, "identity")
	identityStore, err := identity.Open(identity.Config{StateRoot: identityRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := identityStore.SaveRegistration(identity.Registration{
		ServerURL:              backend.URL,
		MachineID:              "mch_source",
		EnvironmentID:          "env_source",
		PublicKeyID:            identityStore.Current().ID,
		PublicIdentityKey:      "test-public-key",
		InboxPath:              filepath.Join(home, "inbox"),
		InstallationGeneration: 1,
		UpdatedAt:              time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.String("config", configPath, "")
	set.String("server", "", "")
	commandContext := command.NewContext(set, strings.NewReader(""))

	// No daemon socket exists: resolution falls back to the server instead of failing.
	client, machine, target, err := resolveSSHCommandTargetWarm(commandContext, "machine-ready")
	if err != nil {
		t.Fatalf("fallback resolve: %v", err)
	}
	if client == nil || machine.ID != "mch_1" || target.Port != 2222 || target.OSUser != "root" {
		t.Fatalf("client=%v machine=%+v target=%+v", client != nil, machine, target)
	}
	if got := atomic.LoadInt32(&machineListCalls); got == 0 {
		t.Fatal("fallback path did not list machines")
	}
	if got := atomic.LoadInt32(&sshTargetCalls); got != 1 {
		t.Fatalf("fallback path fetched SSH target %d times, want 1", got)
	}
}

func TestUpdateStatusReportsDownloadedCandidate(t *testing.T) {
	candidate := &workerupdate.PreparedCandidate{ID: strings.Repeat("a", 64), Version: "2026.09.30.9", Platform: "linux", Architecture: "amd64", SHA256: strings.Repeat("b", 64), Length: 128}
	response := updated.ControlResponse{Candidate: candidate}
	result := updateStatusCommandResult("2026.09.30.8", response, nil)
	if result.Candidate != candidate || result.ActivationPending {
		t.Fatalf("status=%+v", result)
	}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	command.Flags().Bool("json", false, "")
	if err := writeUpdateStatusResult(command, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), candidate.SHA256) || !strings.Contains(output.String(), "pb update --approve "+candidate.ID) || !strings.Contains(output.String(), "interrupts active connections") || strings.Contains(output.String(), "Activation: complete") {
		t.Fatalf("output=%s", output.String())
	}
}

func TestUpdateControlUsesInstalledOwnerNamespace(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var layout service.Layout
	if runtime.GOOS == "windows" {
		layout, err = service.WindowsUserLayout(account.Uid)
	} else {
		uid, parseErr := strconv.Atoi(account.Uid)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		layout, err = service.UserLayout(runtime.GOOS, uid)
	}
	if err != nil {
		t.Fatal(err)
	}
	socket, err := updatedControlSocket()
	if err != nil {
		t.Fatal(err)
	}
	if socket != layout.UpdaterSocket || layout.Instance == "" {
		t.Fatalf("CLI updater endpoint %q differs from installed owner %q endpoint %q", socket, layout.Instance, layout.UpdaterSocket)
	}
}

// This models the narrow remote-owned reference contract consumed by the CLI;
// the tunnel's real wire-decoding test owns validation of incoming metadata.
type referencedTerminalFailure struct{ reference string }

func (e referencedTerminalFailure) Error() string            { return "PRIVATE_REMOTE_TERMINAL_TEXT" }
func (e referencedTerminalFailure) SupportReference() string { return e.reference }

func TestCommandFailureBorrowsTerminalReferenceWithoutHidingIndependentFault(t *testing.T) {
	const ref = "support_01234567-89ab-4def-8123-456789abcdef"
	remote := referencedTerminalFailure{reference: ref}
	failure := classifyCommandFailure(fmt.Errorf("terminal creation: %w", remote))
	if failure.kind != commandOperational || failure.supportReference != ref {
		t.Fatal("captured remote terminal failure lost its ownership")
	}
	message := userFacingError(remote)
	if !strings.Contains(message, ref) || !strings.Contains(message, "pb session list") || strings.Contains(message, "PRIVATE_REMOTE_TERMINAL_TEXT") {
		t.Fatal("remote terminal guidance lost reference or exposed remote prose")
	}
	if failure := classifyCommandFailure(errors.Join(remote, syscall.EIO)); failure.kind != commandUnexpected {
		t.Fatal("remote reference hid independent local fault")
	}
	if failure := classifyCommandFailure(referencedTerminalFailure{reference: "PRIVATE_BAD_REFERENCE"}); failure.kind != commandUnexpected || failure.supportReference != "" {
		t.Fatal("invalid remote reference became trusted ownership")
	}
}

// Round 10 (hp, lanep-r10a/b): a just-paired peer stayed cached offline in the daemon's snapshot until its next
// refresh, and pb exec refused it ("the machine is offline") for ~2 min while ping worked. A cached presence must
// never refuse: a machine the snapshot does not see ready is resolved on the server.
func TestCachedOfflinePeerIsResolvedOnTheServer(t *testing.T) {
	var catalogCalls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/machines" {
			atomic.AddInt32(&catalogCalls, 1)
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "mch_new", "alias": "just-paired", "state": "online", "online": true, "installation_generation": 1, "environment_id": "env_new", "workspace_root": "/root"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
			return
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()
	snapshot := localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 3, ObservedAt: time.Now().UTC(), DaemonState: "ready", DaemonVersion: "dev",
		Machines: []localapi.MachineStatus{{ID: "mch_new", EnvironmentID: "env_new", WorkspaceRoot: "/root", Alias: "just-paired", Eligible: true, RuntimeState: "offline", Generation: 1, SelectedPath: "none", TransferReadiness: "unavailable", PreviewReadiness: "unavailable", SSHReadiness: "unavailable", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown"}},
	}
	previous := loadWarmMachineSnapshot
	loadWarmMachineSnapshot = func(context.Context) (localapi.Snapshot, error) { return snapshot, nil }
	t.Cleanup(func() { loadWarmMachineSnapshot = previous })

	if got, ok := resolveWarmUserMachine(context.Background(), "just-paired"); ok {
		t.Fatalf("a peer cached offline was taken from the snapshot: %+v", got)
	}
	_, machine, err := resolveEnvironmentTargetWithMachine(context.Background(), api.New(backend.URL, config.Credential{AccessToken: "token"}, backend.Client()), "just-paired")
	if err != nil || machine.ID != "mch_new" || !machine.Online {
		t.Fatalf("resolved %+v, %v; want the server's online machine", machine, err)
	}
	if got := atomic.LoadInt32(&catalogCalls); got != 1 {
		t.Fatalf("catalog calls = %d, want 1", got)
	}
}
