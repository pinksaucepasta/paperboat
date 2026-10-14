package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	bugreportpkg "github.com/pinksaucepasta/paperboat/internal/bugreport"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuthStatusRejectsProfileWithMissingSecret(t *testing.T) {
	for _, missing := range []string{"access", "refresh"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			serverURL := "https://api.example.test"
			writeTestProfile(t, dir, configPath, serverURL)
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			store, err := config.ProfileStoreFor(cfg)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := store.Load(serverURL)
			if err != nil {
				t.Fatal(err)
			}
			ref := profile.AccessSecretRef
			if missing == "refresh" {
				ref = profile.RefreshSecretRef
			}
			if err := store.Secrets.Delete(ref); err != nil {
				t.Fatal(err)
			}
			app := newApp()
			var output bytes.Buffer
			app.Writer = &output
			err = app.Run([]string{"pb", "--config", configPath, "auth", "status", "--json"})
			message := userFacingError(err)
			if !errors.Is(err, config.ErrSecretNotFound) || !strings.Contains(message, "`pb login`") || strings.Contains(message, "enrollment") || strings.Contains(message, "paired") || output.Len() != 0 {
				t.Fatal("missing account token did not retain its cause and safe sign-in recovery")
			}
		})
	}
}

func TestAuthStatusPublishesOnlyLocallyTrustedBrowserPairingRoot(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	serverURL := "https://api.example.test"
	isolateCommandCredentialLocation(t, dir)
	profileDir := filepath.Join(dir, "credentials")
	configJSON := `{"server_url":` + quote(serverURL) + `,"auth":{"allow_file_fallback":true,"profile_dir":` + quote(profileDir) + `}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.ProfileStore{Path: profileDir, Secrets: config.FileSecretStore{Dir: filepath.Join(profileDir, "secrets")}}
	expires := time.Now().Add(time.Hour)
	accountID := "account_browser_pairing"
	if err := store.Save(config.Profile{Issuer: serverURL, Account: config.Account{ID: accountID}, CLIClientSessionID: "cls_test", AccessExpiresAt: expires},
		config.Credential{AccessToken: "token", RefreshToken: "refresh", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	public := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if err := store.SavePeerAccountRootPublic(serverURL, accountID, public); err != nil {
		t.Fatal(err)
	}
	root := newApp()
	var output bytes.Buffer
	root.Writer = &output
	if err := root.Run([]string{"pb", "--config", configPath, "auth", "status", "--json"}); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Root        string `json:"trusted_root_public_key"`
		Fingerprint string `json:"trusted_root_fingerprint"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	wantFingerprint, _ := endpointidentity.RootFingerprint(public)
	if document.Root != base64.RawURLEncoding.EncodeToString(public) || document.Fingerprint != wantFingerprint {
		t.Fatalf("trusted browser pairing identity was not exported from local CLI state: %#v", document)
	}
}

func TestAuthLogoutIsLocalEvenWhenRemoteRevocationFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/revoke" {
			http.NotFound(w, r)
			return
		}
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"try again"}}`))
	}))
	defer server.Close()
	writeTestProfile(t, dir, configPath, server.URL)

	if err := newApp().Run([]string{"pb", "--config", configPath, "auth", "logout"}); err != nil {
		t.Fatalf("logout err = %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(server.URL); !errors.Is(err, config.ErrNoCredentials) {
		t.Fatalf("active profile err = %v", err)
	}
	pending, err := store.PendingRevocations(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending revocations = %d", len(pending))
	}
	if attempts != 1 {
		t.Fatalf("remote revocation attempts = %d", attempts)
	}
}

func TestAuthLogoutIgnoresFailedHistoricalRevocationAfterCurrentSucceeds(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/revoke" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer refresh-old" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"old revocation failed"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()
	writeTestProfile(t, dir, configPath, server.URL)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.QueueRevocation(server.URL, "cls_old", "refresh-old"); err != nil {
		t.Fatal(err)
	}
	if err := newApp().Run([]string{"pb", "--config", configPath, "auth", "logout"}); err != nil {
		t.Fatalf("logout err = %v", err)
	}
	if _, err := store.Load(server.URL); !errors.Is(err, config.ErrNoCredentials) {
		t.Fatalf("active profile err = %v", err)
	}
	pending, err := store.PendingRevocations(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending revocations = %#v", pending)
	}
}

func TestDrainPendingRevocationsProcessesMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	store := config.ProfileStore{Path: dir, Secrets: config.FileSecretStore{Dir: filepath.Join(dir, "secrets")}}
	var revoked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revoked = append(revoked, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()
	if err := store.QueueRevocation(server.URL, "cls_old", "refresh-old"); err != nil {
		t.Fatal(err)
	}
	if err := store.QueueRevocation(server.URL, "cls_failed_login", "refresh-new"); err != nil {
		t.Fatal(err)
	}
	if err := drainPendingRevocations(context.Background(), server.URL, store); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 2 {
		t.Fatalf("revoked = %#v", revoked)
	}
	pending, err := store.PendingRevocations(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending revocations = %d", len(pending))
	}
}

func TestForwardedTerminalEnvFiltersInvalidAndUnset(t *testing.T) {
	t.Setenv("PB_TEST_TERM", "xterm-256color")
	t.Setenv("PB_TEST_EMPTY", "")
	env := forwardedTerminalEnv([]string{"PB_TEST_TERM", "PB_TEST_EMPTY", "PB_TEST_UNSET_VAR", "bad-key!"})
	if len(env) != 1 || env["PB_TEST_TERM"] != "xterm-256color" {
		t.Fatalf("env = %#v", env)
	}
}

func TestTerminalHostErrorDistinguishesCapabilityAndAvailability(t *testing.T) {
	receive := api.UserMachine{Online: true}
	var apiErr *api.APIError
	if err := terminalHostError(receive); !errors.As(err, &apiErr) || apiErr.Code != "machine_capability_unavailable" {
		t.Fatalf("receive error = %#v", err)
	}
	host := receive
	host.Capabilities.TerminalHost.Configured = true
	if err := terminalHostError(host); !errors.As(err, &apiErr) || apiErr.Code != "machine_offline" {
		t.Fatalf("offline host error = %#v", err)
	}
	host.Capabilities.TerminalHost.Observed = true
	if err := terminalHostError(host); err != nil {
		t.Fatalf("ready host error = %v", err)
	}
}

func TestSelectStatusMachinePrefersIDAndRejectsAmbiguousAlias(t *testing.T) {
	machines := []localapi.MachineStatus{{ID: "machine_1", Alias: "Studio"}, {ID: "Studio", Alias: "Other"}, {ID: "machine_2", Alias: "studio"}}
	selected, err := selectStatusMachine(machines, "Studio")
	if err != nil || selected.ID != "Studio" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	if _, err := selectStatusMachine(machines, "STUDIO"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous err=%v", err)
	}
	if _, err := selectStatusMachine(machines, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing err=%v", err)
	}
}

func TestWriteStatusIncludesOperationalFieldsAndSafeHealth(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 3, ObservedAt: now, DaemonState: "degraded", DaemonVersion: "dev",
		Health:   []localapi.HealthItem{{Code: "control_plane_unavailable", Severity: "error", Title: "Control plane is unavailable", Recovery: "Check network access", ETag: "control_plane_unavailable"}},
		Machines: []localapi.MachineStatus{{ID: "machine_1", Alias: "Studio Mac", Eligible: true, RuntimeState: "ready", Generation: 4, LastObservedAt: &now, ActiveConsumers: 2, SelectedPath: "relay", RelayRegion: "bom", TransferReadiness: "ready", PreviewReadiness: "degraded", SSHReadiness: "unavailable"}},
	}
	var output bytes.Buffer
	writeStatus(&output, snapshot)
	for _, expected := range []string{"Daemon: degraded", "Control plane is unavailable", "Studio Mac (machine_1)", "Runtime: ready", "Path: relay/bom", "Consumers: 2", "Transfer: ready", "Preview: degraded", "SSH: unavailable"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestDoctorCommandUsesOwnerSocketAndStableJSON(t *testing.T) {
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
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "unconfigured-runtime"))
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/me" || request.Header.Get("Authorization") != "Bearer token" {
			http.Error(writer, "not found", http.StatusNotFound)
			return
		}
		writeAPIData(t, writer, api.Me{ID: "user_1", Email: "owner@example.test"})
	}))
	defer backend.Close()
	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, backend.URL)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "dev", Machines: []localapi.MachineStatus{}}
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
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "doctor", "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var report doctorpkg.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Schema != doctorpkg.SchemaV1 || report.Overall != "degraded" || stderr.Len() != 0 {
		t.Fatalf("report=%#v decode=%v stderr=%q", report, err, stderr.String())
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	checks := make(map[string]doctorpkg.Check, len(report.Checks))
	for _, check := range report.Checks {
		checks[check.Code] = check
	}
	if checks["authentication"].Status != doctorpkg.StatusPass || checks["daemon"].Status != doctorpkg.StatusPass || checks["local_state"].Status != doctorpkg.StatusWarning || checks["udp_ipv4"].Status != doctorpkg.StatusPass {
		t.Fatalf("checks=%#v", checks)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server err=%v", err)
	}
}

func TestWaitCommandUsesLocalWatchAndStableExitResults(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	home := filepath.Join(root, "home")
	runtimeRoot := filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PAPERBOAT_CONFIG", filepath.Join(root, "config", "paperboat.json"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{
		Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "dev",
		Machines: []localapi.MachineStatus{{ID: "machine_1", Alias: "Studio Mac", Eligible: true, RuntimeState: "ready", Generation: 4, SelectedPath: "none", TransferReadiness: "ready", PreviewReadiness: "ready", SSHReadiness: "unavailable", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown"}},
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

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--config", filepath.Join(root, "config", "paperboat.json"), "wait", "Studio Mac", "--for", "runtime", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ready code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var ready localwait.Result
	if err := json.Unmarshal(stdout.Bytes(), &ready); err != nil || ready.Schema != localwait.ResultSchemaV1 || ready.Outcome != "ready" || ready.Machine.ID != "machine_1" {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server err=%v", err)
	}
}

type commandDiagnosticService struct {
	bundle  diagnostics.Bundle
	markers []string
}

func (s *commandDiagnosticService) Diagnostics(context.Context) (localapi.DiagnosticSnapshot, error) {
	return localapi.DiagnosticSnapshot{Schema: localapi.DiagnosticSnapshotSchemaV1, ObservedAt: time.Now().UTC()}, nil
}
func (s *commandDiagnosticService) RecordBugreportMarker(_ context.Context, phase string) error {
	s.markers = append(s.markers, phase)
	return nil
}
func (s *commandDiagnosticService) CreateBugreport(context.Context) (diagnostics.Bundle, error) {
	return s.bundle, nil
}

func TestBugreportCommandUsesDaemonBundleAndStableJSON(t *testing.T) {
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
	now := time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "dev"}
	store, _ := localapi.NewSnapshotStore(&snapshot)
	bundlePath := filepath.Join(root, "bugreport-support_01234567-89ab-4def-8123-456789abcdef.zip")
	content := []byte("PK command bundle")
	if err := atomicfile.Write(bundlePath, content, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		t.Fatal(err)
	}
	diagnosticService := &commandDiagnosticService{bundle: diagnostics.Bundle{Schema: diagnostics.BundleSchemaV1, Correlation: "support_01234567-89ab-4def-8123-456789abcdef", CreatedAt: now, Path: bundlePath, Bytes: int64(len(content)), Categories: []string{"manifest", "recent_events", "redacted_events", "status"}}}
	serverConfig, err := commandLocalAPIServerConfig(paths.SocketPath, store)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig.Diagnostics = diagnosticService
	server, err := localapi.NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	waitForCommandSocket(t, paths.SocketPath)
	var stdout, stderr bytes.Buffer
	command := newRootCommand()
	command.SetIn(bytes.NewBufferString("\n"))
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"bugreport", "--record", "--json"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("execute error=%T %v stdout=%q stderr=%q", err, err, stdout.String(), stderr.String())
	}
	var result bugreportpkg.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Validate() != nil || !result.BundleCreated || !result.Recorded || result.Uploaded || !slices.Equal(diagnosticService.markers, []string{"start", "end"}) {
		t.Fatalf("result=%#v error=%v markers=%v stdout=%s stderr=%s", result, err, diagnosticService.markers, stdout.String(), stderr.String())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server err=%v", err)
	}
}

type bugreportRefreshAuth struct{ calls int }

func (a *bugreportRefreshAuth) Credential() (config.Credential, error) {
	return config.Credential{AccessToken: "stale"}, nil
}

func (a *bugreportRefreshAuth) Refresh() (config.Credential, error) {
	a.calls++
	return config.Credential{AccessToken: "fresh"}, nil
}

func TestRefreshingBugreportServerRetriesAuthorizationWithSameKey(t *testing.T) {
	var keys []string
	expires := time.Now().UTC().Add(time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") == "Bearer stale" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":{"code":"unauthenticated","message":"expired"}}`))
			return
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": api.DiagnosticUploadIntent{Schema: api.DiagnosticUploadIntentSchemaV1, IntentID: "diag_0123456789abcdef", CorrelationID: "support_01234567-89ab-4def-8123-456789abcdef", State: "pending", ExpiresAt: expires, UploadMethod: http.MethodPut, UploadURL: "https://uploads.example.test/bundle", UploadHeaders: map[string]string{"Content-Type": "application/zip"}}})
	}))
	defer server.Close()
	auth := &bugreportRefreshAuth{}
	client, err := newRefreshingBugreportServer(server.URL, auth)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateDiagnosticUploadIntent(context.Background(), "same-operation-key", api.DiagnosticUploadIntentRequest{Schema: api.DiagnosticUploadIntentRequestSchemaV1})
	if err != nil || auth.calls != 1 || !slices.Equal(keys, []string{"same-operation-key", "same-operation-key"}) {
		t.Fatalf("err=%v refreshes=%d keys=%v", err, auth.calls, keys)
	}
}

func TestLocalDaemonSnapshotInstallsOnlyForUnavailableSocket(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	home := filepath.Join(root, "home")
	runtimeRoot := filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server_url":"https://api.paperboat.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConfigPath, configPath)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 14, 0, 0, 0, time.UTC)
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "dev", Machines: []localapi.MachineStatus{}}
	store, _ := localapi.NewSnapshotStore(&snapshot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	installCalls := 0
	installer := func(_ context.Context, executable, configPath, server string) error {
		installCalls++
		if !filepath.IsAbs(executable) || configPath != filepath.Join(root, "config.json") || server != "https://api.paperboat.test" {
			t.Fatalf("executable=%q config=%q server=%q", executable, configPath, server)
		}
		serverConfig, err := commandLocalAPIServerConfig(paths.SocketPath, store)
		if err != nil {
			return err
		}
		localServer, err := localapi.NewServer(serverConfig)
		if err != nil {
			return err
		}
		go func() { done <- localServer.Run(ctx) }()
		return nil
	}
	rootCommand := newRootCommand()
	rootCommand.SetContext(ctx)
	if err := rootCommand.PersistentFlags().Set("config", filepath.Join(root, "custom-config.json")); err != nil {
		t.Fatal(err)
	}
	if err := rootCommand.PersistentFlags().Set("server", "https://api.paperboat.test"); err != nil {
		t.Fatal(err)
	}
	status, _, err := rootCommand.Find([]string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	client, got, err := localDaemonSnapshot(status, installer)
	if err != nil || client == nil || got.Generation != 1 || installCalls != 1 {
		t.Fatalf("client=%v snapshot=%#v calls=%d err=%v", client != nil, got, installCalls, err)
	}
	if _, _, err := localDaemonSnapshot(status, installer); err != nil || installCalls != 1 {
		t.Fatalf("existing daemon calls=%d err=%v", installCalls, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server err=%v", err)
	}
}

func TestCommandUsesTestSafeLocalDaemonInstaller(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	home, runtimeRoot := filepath.Join(root, "home"), filepath.Join(root, "runtime")
	for _, directory := range []string{home, runtimeRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	t.Setenv("TMPDIR", runtimeRoot)
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server_url":"https://api.paperboat.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	previous := installLocalDaemonService
	called := false
	installLocalDaemonService = func(_ context.Context, executable, gotConfigPath, server string) error {
		called = true
		if !strings.HasSuffix(strings.ToLower(executable), ".test") && !strings.HasSuffix(strings.ToLower(executable), ".test.exe") {
			t.Fatalf("expected test executable, got %q", executable)
		}
		if gotConfigPath != configPath || server != "https://api.paperboat.test" {
			t.Fatalf("config=%q server=%q", gotConfigPath, server)
		}
		return errors.New("test installer sentinel")
	}
	t.Cleanup(func() { installLocalDaemonService = previous })

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "status"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !called {
		t.Fatalf("test-safe local daemon installer was not called: stderr=%q", stderr.String())
	}
}

func localDaemonReadinessTestConfig(t *testing.T, registered bool) (*config.Config, string) {
	t.Helper()
	root := commandRuntimeTestRoot(t)
	home, runtimeRoot := filepath.Join(root, "home"), filepath.Join(root, "runtime")
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
	stateRoot := filepath.Join(root, "identity")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", stateRoot)
	configPath := filepath.Join(root, "config.json")
	const serverURL = "https://api.paperboat.test"
	if err := os.WriteFile(configPath, []byte(`{"server_url":"`+serverURL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !registered {
		return cfg, configPath
	}
	identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	inboxPath := filepath.Join(home, "Paperboat Inbox")
	if err := os.MkdirAll(inboxPath, 0o700); err != nil {
		t.Fatal(err)
	}
	key := identityStore.Current()
	if err := identityStore.SaveRegistration(identity.Registration{
		ServerURL: serverURL, MachineID: "machine_local", EnvironmentID: "environment_local",
		PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath: inboxPath, InstallationGeneration: 1, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return cfg, configPath
}

func TestEnsureLocalDaemonServiceDefersUntilMachineRegistration(t *testing.T) {
	cfg, _ := localDaemonReadinessTestConfig(t, false)
	previous := installLocalDaemonService
	calls := 0
	installLocalDaemonService = func(context.Context, string, string, string) error {
		calls++
		return nil
	}
	t.Cleanup(func() { installLocalDaemonService = previous })

	if err := ensureLocalDaemonService(context.Background(), cfg); err != nil {
		t.Fatalf("clean auth readiness error=%v", err)
	}
	if calls != 0 {
		t.Fatalf("clean auth launched an unregistered daemon %d times", calls)
	}
}

func TestEnsureLocalDaemonServiceUsesInstallerAfterRegistration(t *testing.T) {
	cfg, configPath := localDaemonReadinessTestConfig(t, true)
	previous := installLocalDaemonService
	calls := 0
	sentinel := errors.New("test installer sentinel")
	installLocalDaemonService = func(_ context.Context, executable, gotConfigPath, server string) error {
		calls++
		if !strings.HasSuffix(strings.ToLower(executable), ".test") && !strings.HasSuffix(strings.ToLower(executable), ".test.exe") {
			t.Fatalf("expected test executable, got %q", executable)
		}
		if gotConfigPath != configPath || server != "https://api.paperboat.test" {
			t.Fatalf("config=%q server=%q", gotConfigPath, server)
		}
		return sentinel
	}
	t.Cleanup(func() { installLocalDaemonService = previous })

	if err := ensureLocalDaemonService(context.Background(), cfg); !errors.Is(err, sentinel) {
		t.Fatalf("ensure error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("installer calls=%d, want 1", calls)
	}
}

func TestRequireLocalDaemonServiceRejectsMissingRegistrationWithoutLaunching(t *testing.T) {
	cfg, _ := localDaemonReadinessTestConfig(t, false)
	previous := installLocalDaemonService
	installLocalDaemonService = func(context.Context, string, string, string) error {
		t.Fatal("unregistered peer command launched the local daemon")
		return nil
	}
	t.Cleanup(func() { installLocalDaemonService = previous })

	err := requireLocalDaemonService(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "pb setup") {
		t.Fatalf("missing registration error=%v", err)
	}
}

func TestRequireLocalDaemonServiceBoundsMissingReadiness(t *testing.T) {
	cfg, _ := localDaemonReadinessTestConfig(t, true)
	previous := installLocalDaemonService
	calls := 0
	installLocalDaemonService = func(context.Context, string, string, string) error {
		calls++
		return nil
	}
	t.Cleanup(func() { installLocalDaemonService = previous })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := requireLocalDaemonService(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing readiness error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("installer calls=%d, want 1", calls)
	}
}
