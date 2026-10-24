package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMachineHomeActionsFollowConfiguredCapabilities(t *testing.T) {
	machine := api.UserMachine{ID: "machine_actions", Capabilities: api.MachineCapabilities{
		FileReceive: api.MachineCapability{Configured: true}, PreviewLaunch: api.MachineCapability{Configured: true},
	}}
	actions := machineHomeActions(machine)
	ids := make([]string, len(actions))
	for index, action := range actions {
		ids[index] = action.ID
	}
	if !slices.Equal(ids, []string{"rename", "send", "allow-sleep", "keep-awake"}) {
		t.Fatalf("receive actions=%v", ids)
	}
	machine.Capabilities.TerminalHost.Configured = true
	machine.Capabilities.EnvironmentInjection.Configured = true
	actions = machineHomeActions(machine)
	ids = ids[:0]
	for _, action := range actions {
		ids = append(ids, action.ID)
	}
	if !slices.Equal(ids, []string{"rename", "environment-variables", "terminal", "send", "sessions", "allow-sleep", "keep-awake"}) {
		t.Fatalf("host actions=%v", ids)
	}
}

func TestMachineStatusSummaryShowsIncomingCapabilityCount(t *testing.T) {
	tests := []struct {
		name    string
		machine api.UserMachine
		want    string
	}{
		{
			name:    "online defaults",
			machine: api.UserMachine{Online: true, MachineCapabilities: api.MachineCapabilityPolicy{Desired: api.MachineCapabilitySelection{Terminal: true, ManagedSSH: true, FileReceive: true, PreviewTunnel: true}}},
			want:    "Online  ·  4 incoming services",
		},
		{
			name:    "offline disabled",
			machine: api.UserMachine{},
			want:    "Offline  ·  0 incoming services",
		},
		{
			name:    "partial incoming services",
			machine: api.UserMachine{MachineCapabilities: api.MachineCapabilityPolicy{Desired: api.MachineCapabilitySelection{Terminal: true, FileReceive: true}}},
			want:    "Offline  ·  2 incoming services",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := machineStatusSummary(test.machine); got != test.want {
				t.Fatalf("summary=%q want=%q", got, test.want)
			}
		})
	}
}

func TestAsyncHomeValueStartsImmediatelyAndTracksFreshness(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	value := startAsyncHomeValue(context.Background(), func(context.Context) (string, error) {
		close(started)
		<-release
		return "ready", nil
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background load did not start")
	}
	if value.ready() {
		t.Fatal("blocked load reported ready")
	}
	close(release)
	got, err := value.await(context.Background())
	if err != nil || got != "ready" || !value.fresh() {
		t.Fatalf("value=%q err=%v fresh=%t", got, err, value.fresh())
	}
	value.fetchedAt = time.Now().Add(-homePrefetchFreshness - time.Second)
	if value.fresh() {
		t.Fatal("expired prefetch remained fresh")
	}
}

func TestMachinesSortCurrentMachineLast(t *testing.T) {
	machines := []api.UserMachine{{ID: "current", Alias: "local"}, {ID: "favorite", Alias: "favorite"}, {ID: "remote", Alias: "remote"}}
	favorites := favoriteSet{}
	favorites.Set("machine", "current", true)
	favorites.Set("machine", "favorite", true)
	sortMachinesForDisplay(machines, favorites, "current")
	if got := []string{machines[0].ID, machines[1].ID, machines[2].ID}; !slices.Equal(got, []string{"favorite", "remote", "current"}) {
		t.Fatalf("machine order=%v", got)
	}
	if got := machineDisplayTitle(machines[2], "current"); got != "local (this machine)" {
		t.Fatalf("current title=%q", got)
	}
}

func TestDroppedFilePathsParsesQuotedAndEscapedPaths(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first file.txt")
	second := filepath.Join(directory, "second.txt")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	value := strconv.Quote(first) + " " + strings.ReplaceAll(second, " ", "\\ ")
	paths, err := droppedFilePaths(value)
	if err != nil || !slices.Equal(paths, []string{first, second}) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	if _, err := droppedFilePaths(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing dropped file was accepted")
	}
}

func TestFailedUnifiedSetupPreservesMachineControlCapabilitiesAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name              string
		flags             []string
		failCapabilities  bool
		initial, expected api.MachineCapabilitySelection
	}{
		{name: "retry preserves explicit choices", initial: api.MachineCapabilitySelection{Terminal: true, FileReceive: true}, expected: api.MachineCapabilitySelection{Terminal: true, FileReceive: true}},
		{name: "explicit flags override selected choices", flags: []string{"--terminal=true", "--managed-ssh=false"}, initial: api.MachineCapabilitySelection{ManagedSSH: true, FileReceive: true}, expected: api.MachineCapabilitySelection{Terminal: true, FileReceive: true}},
		{name: "server defaults remain enabled", initial: api.MachineCapabilitySelection{Terminal: true, ManagedSSH: true, FileReceive: true, PreviewTunnel: true}, expected: api.MachineCapabilitySelection{Terminal: true, ManagedSSH: true, FileReceive: true, PreviewTunnel: true}},
		{name: "capability save failure is retryable", failCapabilities: true, initial: api.MachineCapabilitySelection{FileReceive: true}, expected: api.MachineCapabilitySelection{FileReceive: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := commandRuntimeTestRoot(t)
			stateRoot := filepath.Join(root, "identity")
			t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
			t.Setenv("XDG_RUNTIME_DIR", root)
			store, err := identity.Open(identity.Config{StateRoot: stateRoot})
			if err != nil {
				t.Fatal(err)
			}
			key := store.Current()
			publicKey := base64.RawURLEncoding.EncodeToString(key.Public())
			inboxPath := filepath.Join(root, "Inbox")
			var setupCalls, controlCalls, capabilitiesCalls atomic.Int32
			desired := test.initial
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/setup":
					setupCalls.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": api.UserMachine{
						ID: "machine_preserved", EnvironmentID: "machine_preserved", Alias: "studio", PublicIdentityKey: publicKey, InstallationGeneration: 5,
						MachineCapabilities: api.MachineCapabilityPolicy{Desired: desired, DesiredVersion: 3},
						Installation:        &api.ClientInstallation{ControlURL: "https://control.example.test", HelperListenAddress: "127.0.0.1:38080"},
					}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/machines/machine_preserved/control-credentials":
					controlCalls.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": api.MachineControlCredential{Credential: strings.Repeat("c", 48), ExpiresAt: time.Now().Add(time.Hour)}})
				case r.Method == http.MethodPut && r.URL.Path == "/v1/machines/machine_preserved/capabilities":
					capabilitiesCalls.Add(1)
					var input struct {
						Desired         api.MachineCapabilitySelection `json:"desired"`
						ExpectedVersion int64                          `json:"expected_version"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Desired != test.expected || input.ExpectedVersion != 3 {
						t.Errorf("capabilities changed unexpectedly: %+v", input)
					}
					if test.failCapabilities {
						w.WriteHeader(http.StatusServiceUnavailable)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "unavailable", "message": "capability save failed"}})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": api.MachineCapabilityPolicy{Desired: test.expected, DesiredVersion: 4}})
				default:
					t.Errorf("unexpected recovery server mutation %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			previous := identity.Registration{AccountID: "account_preserved", ServerURL: server.URL, MachineID: "machine_preserved", EnvironmentID: "machine_preserved", PublicKeyID: key.ID, PublicIdentityKey: publicKey, InboxPath: inboxPath, InstallationGeneration: 5, SSHUser: "known-user", SSHPort: 22, UpdatedAt: time.Now()}
			if err := store.SaveRegistration(previous); err != nil {
				t.Fatal(err)
			}
			resume := bootstrap.NewResumeRecord(server.URL, publicKey, "", "studio", "verifier-012345678901234567890123456789", time.Now().Add(time.Hour))
			resume.AuthenticatedSetup = true
			resume.SetupOperationID = "machine-operation"
			resume.ExpectedUserMachineID = previous.MachineID
			resume.ExpectedGeneration = 5
			resume.PairingStarted = true
			if err := bootstrap.SaveResume(stateRoot, resume); err != nil {
				t.Fatal(err)
			}
			resumeBefore, err := os.ReadFile(bootstrap.ResumePath(stateRoot))
			if err != nil {
				t.Fatal(err)
			}
			custodyPath := filepath.Join(stateRoot, "custody-fixture")
			if err := os.WriteFile(custodyPath, []byte("existing protected custody"), 0600); err != nil {
				t.Fatal(err)
			}
			oldVerifier := verifySetupInstallSource
			oldBackend := setupBackendClient
			setupBackendClient = func(*command.Context) (*api.Client, error) {
				return api.New(server.URL, config.Credential{AccessToken: "fixture-token"}, server.Client()), nil
			}
			sourceFailure := errors.New("invalid supplied executable")
			verifySetupInstallSource = func() error { return sourceFailure }
			t.Cleanup(func() { verifySetupInstallSource = oldVerifier; setupBackendClient = oldBackend })
			configPath := filepath.Join(root, "config.json")
			writeTestProfile(t, root, configPath, server.URL)
			command := newRootCommand()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(append([]string{"--config", configPath, "setup", "--state-root", stateRoot, "--name", "studio"}, test.flags...))
			err = command.ExecuteContext(context.Background())
			var capabilityErr *api.APIError
			if err == nil || (!test.failCapabilities && !errors.Is(err, sourceFailure)) || (test.failCapabilities && (!errors.As(err, &capabilityErr) || capabilityErr.Status != http.StatusServiceUnavailable)) || !strings.Contains(err.Error(), "retry `pb setup`") {
				t.Fatalf("failure=%v", err)
			}
			registration, err := store.Registration()
			if err != nil || registration.AccountID != previous.AccountID || registration.MachineID != previous.MachineID || registration.EnvironmentID != previous.EnvironmentID || registration.PublicIdentityKey != publicKey || registration.InstallationGeneration != 5 || registration.InboxPath != inboxPath || registration.SSHUser != previous.SSHUser || registration.SSHPort != previous.SSHPort {
				t.Fatalf("registration=%+v err=%v", registration, err)
			}
			control, err := store.MachineControl(time.Now(), 0)
			if err != nil || control.InstallationGeneration != 5 || control.MachineID != previous.MachineID {
				t.Fatalf("control=%+v err=%v", control, err)
			}
			resumeAfter, err := os.ReadFile(bootstrap.ResumePath(stateRoot))
			if err != nil || !bytes.Equal(resumeBefore, resumeAfter) {
				t.Fatalf("recovery operation changed or cleared: %v", err)
			}
			custodyAfter, err := os.ReadFile(custodyPath)
			if err != nil || string(custodyAfter) != "existing protected custody" {
				t.Fatalf("custody changed: %v", err)
			}
			if setupCalls.Load() != 1 || controlCalls.Load() != 1 || capabilitiesCalls.Load() != 1 {
				t.Fatalf("unexpected repeated mutations setup=%d control=%d capabilities=%d", setupCalls.Load(), controlCalls.Load(), capabilitiesCalls.Load())
			}
		})
	}
}

func TestConfigCommandsAreDiscoverableAndUnassignRequiresConfirmation(t *testing.T) {
	root := newRootCommand()
	for _, path := range []string{"config sync path", "config sync init", "config sync validate", "config sync apply", "config unassign", "config show", "config path"} {
		command, _, err := root.Find(strings.Fields(path))
		if err != nil || command.CommandPath() != "pb "+path {
			t.Fatalf("find %q command=%v err=%v", path, command, err)
		}
	}
	command, _, err := root.Find([]string{"config", "unassign"})
	if err != nil || command.Flags().Lookup("confirm") == nil || command.Flags().Lookup("yes") != nil {
		t.Fatalf("confirmation flags: command=%v err=%v", command, err)
	}
}

func TestCompatibilityOnlyCommandsAreAbsent(t *testing.T) {
	root := newRootCommand()
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetErr(&help)
	root.SetArgs([]string{"--help"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("help err=%v", err)
	}
	if strings.Contains(strings.ToLower(help.String()), "e2ee") || strings.Contains(strings.ToLower(help.String()), "noise") {
		t.Fatalf("public help exposes private transport internals: %q", help.String())
	}
	root = newRootCommand()
	for _, child := range root.Commands() {
		if child.Name() == "e2ee" || child.Name() == "codex" || child.Name() == "opencode" {
			t.Fatalf("removed/deferred command %q is still exposed", child.Name())
		}
	}
	for parentName, removed := range map[string][]string{
		"config":   {"enable", "disable"},
		"preview":  {"create", "revoke"},
		"session":  {"purge"},
		"sessions": {"purge"},
	} {
		parent, _, err := root.Find([]string{parentName})
		if err != nil {
			t.Fatal(err)
		}
		for _, removedName := range removed {
			for _, child := range parent.Commands() {
				if child.Name() == removedName {
					t.Fatalf("compatibility-only command %q is still registered under %q", removedName, parentName)
				}
			}
		}
	}
}

func TestPrivateTransportLifecycleIsIntegratedIntoPublicWorkflows(t *testing.T) {
	root := newRootCommand()
	for _, path := range []string{"auth login", "setup"} {
		entry, _, err := root.Find(strings.Fields(path))
		if err != nil || entry.CommandPath() != "pb "+path {
			t.Fatalf("find %q command=%v err=%v", path, entry, err)
		}
	}
	login, _, _ := root.Find([]string{"auth", "login"})
	if login.Flags().Lookup("recovery-key") != nil {
		t.Fatal("login must not expose a recovery workflow")
	}
	setup, _, _ := root.Find([]string{"setup"})
	if setup.Flags().Lookup("recovery-output") == nil {
		t.Fatal("setup does not own recovery backup")
	}
}

func TestConfigSetRejectsRemovedTerminalProfile(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "config.json"), "config", "set", "terminal-profile", "full"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "usage: pb config set server <url>") {
		t.Fatalf("err=%v", err)
	}
}

func TestStatusBarConfigCommandsPersistValidatePreviewAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{
		{"--config", path, "config", "status-bar", "set", "theme", "dark"},
		{"--config", path, "config", "status-bar", "set", "privacy", "true"},
		{"--config", path, "config", "status-bar", "set", "center", "none"},
		{"--config", path, "config", "status-bar", "set", "right", "activity,connection"},
	} {
		if code := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("%v exited %d", args, code)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StatusBar.Theme != "dark" || !cfg.StatusBar.Privacy || strings.Join(cfg.StatusBar.Right, ",") != "activity,connection" {
		t.Fatalf("persisted status bar = %+v", cfg.StatusBar)
	}
	var preview bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "config", "status-bar", "preview", "--width", "60"}, &preview, &preview); code != 0 {
		t.Fatalf("preview exited %d: %q", code, preview.String())
	}
	if lines := strings.Count(strings.TrimSpace(preview.String()), "\n") + 1; lines != 3 {
		t.Fatalf("preview lines = %d: %q", lines, preview.String())
	}
	var invalid bytes.Buffer
	if code := run(context.Background(), []string{"--config", path, "config", "status-bar", "set", "accent", "not-a-color"}, &invalid, &invalid); code != 2 {
		t.Fatalf("invalid color exited %d: %q", code, invalid.String())
	}
	if code := run(context.Background(), []string{"--config", path, "config", "status-bar", "reset"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("reset exited %d", code)
	}
	cfg, err = config.Load(path)
	if err != nil || cfg.StatusBar.Theme != config.DefaultStatusBarTheme || cfg.StatusBar.Privacy {
		t.Fatalf("reset status bar = %+v err=%v", cfg.StatusBar, err)
	}
}

func TestConnectStatusBarOverridesAreValidated(t *testing.T) {
	var output bytes.Buffer
	if code := run(context.Background(), []string{"connect", "demo", "--status-bar-theme", "rainbow"}, &output, &output); code != 2 {
		t.Fatalf("exit=%d output=%q", code, output.String())
	}
}

func TestTerminalDebugFlagIsAvailableOnShorthandAndConnect(t *testing.T) {
	root := newRootCommand()
	if flag := root.Flags().Lookup("debug"); flag == nil || flag.DefValue != "false" {
		t.Fatalf("root debug flag = %#v", flag)
	}
	connect, _, err := root.Find([]string{"connect"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := connect.Flags().Lookup("debug"); flag == nil || flag.DefValue != "false" {
		t.Fatalf("connect debug flag = %#v", flag)
	}
	attach, _, err := root.Find([]string{"session", "attach"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := attach.Flags().Lookup("debug"); flag == nil || flag.DefValue != "false" {
		t.Fatalf("session attach debug flag = %#v", flag)
	}
}

func TestMachineRevokeJSONOutputContract(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	var disconnected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "studio"}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case "POST /v1/machines/um_1/disconnect":
			disconnected = true
			writeAPIData(t, w, map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)
	var output bytes.Buffer
	if code := runWithConfirmation(t, []string{"--config", configPath, "machine", "revoke", "studio", "--json"}, &output, &output); code != 0 {
		t.Fatalf("exit code=%d output=%q", code, output.String())
	}
	if !disconnected {
		t.Fatal("disconnect endpoint was not called")
	}
	var got struct {
		Version     string `json:"version"`
		UserMachine struct {
			ID    string `json:"id"`
			Alias string `json:"alias"`
			State string `json:"state"`
		} `json:"machine"`
		Outcome string `json:"outcome"`
		Retry   string `json:"retry"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode output %q: %v", output.String(), err)
	}
	if got.Version != "1" || got.UserMachine.ID != "um_1" || got.UserMachine.Alias != "studio" || got.UserMachine.State != "disconnected" || got.Outcome != "confirmed" || got.Retry != "not_required" {
		t.Fatalf("output=%s", output.String())
	}
}

func TestMachineAddPrintsOneShotEnrollmentCommands(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/client-configuration" {
			writeAPIData(t, w, map[string]string{"version": "1", "machines_url": "https://console.example.test/machines", "installer_base_url": "https://install.example.test/custom"})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machine-enrollments" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		token := "posix-token"
		if body["shell"] == "powershell" {
			token = "windows-token"
		}
		writeAPIData(t, w, api.MachineEnrollmentStart{ID: "ume_" + body["shell"], BootstrapToken: token, ServerURL: "https://api.example.test"})
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "machine", "add", "--name", "Victus"}, &output, &output); code != 0 {
		t.Fatalf("exit=%d output=%q", code, output.String())
	}
	if !strings.Contains(output.String(), "Victus-posix-token") || !strings.Contains(output.String(), "install.example.test/custom/install?p=Victus-windows-token") || !strings.Contains(output.String(), "PowerShell or Command Prompt") || !strings.Contains(output.String(), `powershell -c "iex (irm '`) || !strings.Contains(output.String(), `')"`) || strings.Contains(output.String(), `| iex`) || strings.Contains(output.String(), "iwr '") || strings.Contains(output.String(), "-OutFile") || strings.Contains(output.String(), "powershell -NoLogo") || strings.Contains(output.String(), "--setup-mode") || strings.Contains(output.String(), "PAPERBOAT_SERVER") {
		t.Fatalf("output=%q", output.String())
	}
}

func TestWindowsEnrollmentCommandRunsFromCommandPrompt(t *testing.T) {
	const installerURL = "https://get.example.test/install?p=rpi-0123456789ABCDEFGHIJKLMNOP"
	want := `powershell -c "iex (irm 'https://get.example.test/install?p=rpi-0123456789ABCDEFGHIJKLMNOP')"`
	if got := windowsEnrollmentCommand(installerURL); got != want {
		t.Fatalf("windows enrollment command=%q, want %q", got, want)
	}
}

func TestDefaultEnvironmentRevalidatesRememberedIDWithinSelectedWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/machines" || r.URL.Query().Get("workspace") != "team-a" {
			t.Fatalf("request = %s %s, want machine inventory scoped to team-a", r.Method, r.URL.RequestURI())
		}
		writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "studio", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	got, err := defaultEnvironment(context.Background(), client, "um_1", "team-a")
	if err != nil || got != "um_1" {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestDefaultEnvironmentSelectsOnlyAvailableTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "studio", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if err := client.SetWorkspace("personal"); err != nil {
		t.Fatal(err)
	}
	got, err := defaultEnvironment(context.Background(), client, "", "personal")
	if err != nil || got != "um_1" {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestRootWithoutArgumentsAttemptsPrimaryWorkflow(t *testing.T) {
	var output bytes.Buffer
	app := newApp()
	app.Writer = &output
	app.ErrWriter = &output
	err := app.Run([]string{"pb", "--config", filepath.Join(t.TempDir(), "config.json")})
	if err == nil {
		t.Fatal("expected an actionable setup error")
	}
	if strings.Contains(output.String(), "Usage:") {
		t.Fatalf("root stopped at generic help: %q", output.String())
	}
}

func TestCobraRootWithoutArgumentsAttemptsPrimaryWorkflow(t *testing.T) {
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "config.json")}, &output, &output); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if strings.Contains(output.String(), "Usage:") {
		t.Fatalf("root stopped at generic help: %q", output.String())
	}
}

func TestConnectDoesNotExposeSessionOverrides(t *testing.T) {
	for _, flag := range []string{"--size", "--agent"} {
		t.Run(flag, func(t *testing.T) {
			err := newApp().Run([]string{"pb", flag, "value", "demo"})
			if err == nil || classifyCommandFailure(err).kind != commandUsage {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestConnectWithoutServerDoesNotRunLocalShell(t *testing.T) {
	previousDefault := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = ""
	t.Cleanup(func() { buildinfo.DefaultServerURL = previousDefault })
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := newApp().Run([]string{"pb", "--config", configPath, "demo"})
	if err == nil || !strings.Contains(err.Error(), "server is not configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestDoctorReturnsFailureWhenBackendIsUnconfigured(t *testing.T) {
	previousDefault := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = ""
	t.Cleanup(func() { buildinfo.DefaultServerURL = previousDefault })
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"connect":{"ready_timeout_seconds":30,"poll_interval_seconds":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newApp().Run([]string{"pb", "--config", path, "doctor"}); err == nil {
		t.Fatal("doctor returned success for missing server")
	}
}

func TestDoctorProxyDiagnosisIsTypedAndActionable(t *testing.T) {
	for name, test := range map[string]struct {
		failure httptransport.ProxyFailure
		state   string
		want    string
	}{
		"pac":            {httptransport.ProxyAutomaticConfigurationUnsupported, "pac_unsupported", "HTTPS_PROXY"},
		"authentication": {httptransport.ProxyAuthenticationRequired, "authentication_required", "credential-free"},
		"invalid":        {httptransport.ProxyInvalid, "invalid_configuration", "no path"},
	} {
		t.Run(name, func(t *testing.T) {
			diagnosis, ok := doctorProxyDiagnosis(fmt.Errorf("backend: %w", &httptransport.ProxyError{Failure: test.failure}))
			if !ok || diagnosis.State != test.state || !strings.Contains(diagnosis.Recovery, test.want) {
				t.Fatalf("diagnosis=%+v ok=%v", diagnosis, ok)
			}
		})
	}
	if diagnosis, ok := doctorProxyDiagnosis(errors.New("network unavailable")); ok || diagnosis != (proxyDoctorDiagnosis{}) {
		t.Fatalf("diagnosis=%+v ok=%v", diagnosis, ok)
	}
}

func TestCobraAcceptsPersistentFlagsAfterNestedCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var output bytes.Buffer
	if code := run(context.Background(), []string{"config", "path", "--server", "https://api.example.com", "--config", path}, &output, &output); code != 0 {
		t.Fatalf("exit code = %d, output = %q", code, output.String())
	}
}

func TestCobraParsesNestedSessionFlagsWithoutRewriting(t *testing.T) {
	var output bytes.Buffer
	code := run(context.Background(), []string{"session", "delete", "demo", "api", "--server", "http://127.0.0.1:1"}, &output, &output)
	if code != 1 || strings.Contains(output.String(), "unknown flag") {
		t.Fatalf("exit code = %d output = %q", code, output.String())
	}
}

func TestRemovedPublicCommands(t *testing.T) {
	root := newRootCommand()
	for _, removed := range []string{"desktop", "sessions", "device"} {
		for _, command := range root.Commands() {
			if command.Name() == removed {
				t.Fatalf("public command %q remains registered", removed)
			}
		}
	}
	internal, _, err := root.Find([]string{"__desktop", "request"})
	if err != nil || internal == nil || internal.Name() != "request" || !internal.Parent().Hidden {
		t.Fatalf("hidden desktop bridge unavailable: command=%v error=%v", internal, err)
	}
	for _, path := range [][]string{{"machine", "list"}, {"team", "machine", "grant"}, {"service", "status"}} {
		command, _, err := root.Find(path)
		if err != nil || command == nil || command.Name() != path[len(path)-1] {
			t.Fatalf("command %v unavailable: %v", path, err)
		}
	}
	team, _, err := root.Find([]string{"team"})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range team.Commands() {
		if command.Name() == "device" {
			t.Fatal("old team device command remains registered")
		}
	}
	daemon, _, err := root.Find([]string{"daemon"})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range daemon.Commands() {
		if command.Name() == "service" {
			t.Fatal("duplicate daemon service command remains registered")
		}
	}
}

func TestCobraUsageErrorsReturnExitCodeTwo(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "unknown"},
		{"connect", "demo", "--", "--new"},
		{"demo", "--new", "--session", "api"},
		{"config", "path", "extra"},
	} {
		var output bytes.Buffer
		if code := run(context.Background(), args, &output, &output); code != 2 {
			t.Fatalf("args=%q exit code=%d output=%q", args, code, output.String())
		}
		if !strings.Contains(output.String(), "Usage:") {
			t.Fatalf("args=%q missing usage: %q", args, output.String())
		}
	}
}

func TestBareServerFlagPersistsNormalizedServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if code := run(context.Background(), []string{"--config", path, "--server", "https://api.example.com/"}, os.Stdout, os.Stderr); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != "https://api.example.com" {
		t.Fatalf("server URL = %q", cfg.ServerURL)
	}
}

func writeAuthLoginTestProfile(t *testing.T, root, configPath, issuer string) config.ProfileStore {
	t.Helper()
	isolateCommandCredentialLocation(t, root)
	profileDir := filepath.Join(root, "credentials")
	configJSON := `{"server_url":` + quote(issuer) + `,"auth":{"allow_file_fallback":true,"profile_dir":` + quote(profileDir) + `}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.ProfileStore{Path: profileDir, Secrets: config.FileSecretStore{Dir: filepath.Join(profileDir, "secrets")}}
	expires := time.Now().UTC().Add(time.Hour)
	profile := config.Profile{Issuer: issuer, Account: config.Account{ID: "account_1", Email: "old@example.test"}, CLIClientSessionID: "cls_old", AccessExpiresAt: expires}
	credential := config.Credential{AccessToken: "access-old", RefreshToken: "refresh-old", TokenType: "Bearer", ExpiresAt: expires}
	if err := store.Save(profile, credential); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSessionNameReservesOnlyAutomaticShellNames(t *testing.T) {
	if err := validateSessionName("shell-tools"); err != nil {
		t.Fatalf("shell-tools should be valid: %v", err)
	}
	if err := validateSessionName("shell-2"); err == nil {
		t.Fatal("shell-2 should be reserved")
	}
}

func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}

func writeTestProfile(t *testing.T, dir, configPath, serverURL string) {
	t.Helper()
	isolateCommandCredentialLocation(t, dir)
	profileDir := filepath.Join(dir, "credentials")
	configJSON := `{"server_url":` + quote(serverURL) + `,"auth":{"allow_file_fallback":true,"profile_dir":` + quote(profileDir) + `},"connect":{"ready_timeout_seconds":30,"poll_interval_seconds":1,"dial_retries":0}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.ProfileStore{Path: profileDir, Secrets: config.FileSecretStore{Dir: filepath.Join(profileDir, "secrets")}}
	expires := time.Now().Add(time.Hour)
	err := store.Save(config.Profile{Issuer: serverURL, CLIClientSessionID: "cls_test", AccessExpiresAt: expires}, config.Credential{AccessToken: "token", RefreshToken: "refresh", ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
}

// Round 26: revoking a machine that was already disconnected failed with operation_failed/local_io,
// because the default machine list hides disconnected machines. It is reported as already done.
func TestMachineRevokeOfADisconnectedMachineIsAlreadyDone(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/machines":
			items := []map[string]any{}
			if r.URL.Query().Get("state") == "disconnected" {
				items = []map[string]any{{"id": "um_gone", "alias": "studio", "state": "disconnected"}}
			}
			writeAPIData(t, w, map[string]any{"items": items, "pagination": map[string]any{"limit": 200, "offset": 0, "total": len(items), "next_offset": nil}})
		case "GET /v1/workspaces":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		case "POST /v1/machines/um_gone/disconnect":
			t.Fatal("an already disconnected machine was disconnected again")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)
	var output bytes.Buffer
	if code := run(t.Context(), []string{"--config", configPath, "machine", "revoke", "um_gone", "--json"}, &output, &output); code != 0 {
		t.Fatalf("exit code=%d output=%q", code, output.String())
	}
	if !strings.Contains(output.String(), `"outcome":"already_disconnected"`) || !strings.Contains(output.String(), `"id":"um_gone"`) {
		t.Fatalf("output=%s", output.String())
	}
}
