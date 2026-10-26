package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func testSSHTargetCacheMachine() api.UserMachine {
	return api.UserMachine{ID: "mch_1", Alias: "machine-ready", InstallationGeneration: 4, EnvironmentID: "env_1", WorkspaceRoot: "/root"}
}

func isolateSSHTargetCache(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	isolateCommandCredentialLocation(t, root)
	if err := os.MkdirAll(filepath.Join(root, "paperboat"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestSSHTargetCacheRoundTrip(t *testing.T) {
	isolateSSHTargetCache(t)
	cfg := &config.Config{ServerURL: "https://api.paperboat.test"}
	machine := testSSHTargetCacheMachine()
	now := time.Unix(1_800_000_000, 0)
	if _, ok, err := sshTargetCacheLookup(cfg, machine, now); err != nil || ok {
		t.Fatalf("absent cache lookup = %t, %v", ok, err)
	}
	target := api.ManagedSSHTarget{MachineID: machine.ID, MachineGeneration: 4, OSUser: "root", Port: 2222}
	if err := sshTargetCacheStore(cfg, machine, target, now); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, ok, err := sshTargetCacheLookup(cfg, machine, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok || got.OSUser != "root" || got.Port != 2222 {
		t.Fatalf("lookup = %+v, %t", got, ok)
	}
}

func TestSSHTargetCacheQuietlyMissesUnknownVersion(t *testing.T) {
	isolateSSHTargetCache(t)
	cfg := &config.Config{ServerURL: "https://api.paperboat.test"}
	machine := testSSHTargetCacheMachine()
	path, err := sshTargetCacheFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sshTargetCache{Version: sshTargetCacheVersion + 1, Entries: map[string]sshTargetCacheEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := sshTargetCacheLookup(cfg, machine, time.Now()); err != nil || ok {
		t.Fatalf("unknown-version cache lookup = %t, %v", ok, err)
	}
}

func TestSSHTargetCacheExpiresAfterTTL(t *testing.T) {
	isolateSSHTargetCache(t)
	cfg := &config.Config{ServerURL: "https://api.paperboat.test"}
	machine := testSSHTargetCacheMachine()
	now := time.Unix(1_800_000_000, 0)
	if err := sshTargetCacheStore(cfg, machine, api.ManagedSSHTarget{OSUser: "root", Port: 2222}, now); err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, ok, err := sshTargetCacheLookup(cfg, machine, now.Add(sshTargetCacheTTL+time.Second)); err != nil || ok {
		t.Fatal("stale cache entry was accepted")
	}
}

func TestSSHTargetCacheRejectsGenerationChange(t *testing.T) {
	isolateSSHTargetCache(t)
	cfg := &config.Config{ServerURL: "https://api.paperboat.test"}
	machine := testSSHTargetCacheMachine()
	now := time.Unix(1_800_000_000, 0)
	if err := sshTargetCacheStore(cfg, machine, api.ManagedSSHTarget{OSUser: "root", Port: 2222}, now); err != nil {
		t.Fatalf("store: %v", err)
	}
	reinstalled := machine
	reinstalled.InstallationGeneration = 5
	if _, ok, err := sshTargetCacheLookup(cfg, reinstalled, now.Add(time.Minute)); err != nil || ok {
		t.Fatal("cache entry from a previous generation was accepted")
	}
}

func TestSSHTargetCacheRejectsDifferentServer(t *testing.T) {
	isolateSSHTargetCache(t)
	cfg := &config.Config{ServerURL: "https://api.paperboat.test"}
	machine := testSSHTargetCacheMachine()
	now := time.Unix(1_800_000_000, 0)
	if err := sshTargetCacheStore(cfg, machine, api.ManagedSSHTarget{OSUser: "root", Port: 2222}, now); err != nil {
		t.Fatalf("store: %v", err)
	}
	other := &config.Config{ServerURL: "https://other.paperboat.test"}
	if _, ok, err := sshTargetCacheLookup(other, machine, now.Add(time.Minute)); err != nil || ok {
		t.Fatal("cache entry from another server was accepted")
	}
}

func TestResolveSSHCommandTargetReportsAndRepairsCorruptCache(t *testing.T) {
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
	identityRoot := filepath.Join(root, "identity")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", identityRoot)

	var machineListCalls, sshTargetCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			machineListCalls.Add(1)
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "mch_1", "alias": "machine-ready", "state": "online", "online": true, "installation_generation": 4, "environment_id": "env_1", "workspace_root": "/root", "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/mch_1/ssh-target":
			sshTargetCalls.Add(1)
			writeAPIData(t, w, map[string]any{"type": "machine_target", "version": 1, "machine_id": "mch_1", "machine_generation": 4, "os_user": "root", "port": 2222, "reconciliation_version": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, backend.URL)
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

	previousSnapshotLoader := loadWarmMachineSnapshot
	loadWarmMachineSnapshot = func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{
			Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: time.Now(), DaemonState: "ready", DaemonVersion: "test",
			Machines: []localapi.MachineStatus{{ID: "mch_1", EnvironmentID: "env_1", WorkspaceRoot: "/root", Alias: "machine-ready", Eligible: true, RuntimeState: "ready", Generation: 4, SelectedPath: "none", TransferReadiness: "unavailable", PreviewReadiness: "unavailable", SSHReadiness: "ready", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown", UpdateHealth: "unknown"}},
		}, nil
	}
	t.Cleanup(func() { loadWarmMachineSnapshot = previousSnapshotLoader })

	cfg := &config.Config{ServerURL: backend.URL}
	machine := testSSHTargetCacheMachine()
	cachePath, err := sshTargetCacheFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	const privateCachePayload = `{"access_token":"CACHE_PRIVATE_SENTINEL"`
	if err := os.WriteFile(cachePath, []byte(privateCachePayload), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, cause := sshTargetCacheLookup(cfg, machine, time.Now())
	if cause == nil || cause.Error() != "SSH target cache is unavailable" {
		t.Fatalf("cache failure presentation = %v", cause)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(cause, &syntaxErr) {
		t.Fatalf("cache failure lost JSON parser cause: %T", cause)
	}

	var observed []errorreport.Fault
	restoreReporter := errorreport.Install(nil)
	defer restoreReporter()
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		observed = append(observed, fault)
	})
	defer restoreObserver()
	reference := supportref.New()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.String("config", configPath, "")
	set.String("server", "", "")
	commandContext := command.NewContext(set, strings.NewReader(""))
	commandContext.Context = supportref.WithContext(context.Background(), reference)

	client, selected, target, err := resolveSSHCommandTargetFast(commandContext, "machine-ready")
	if err != nil {
		t.Fatalf("resolve with corrupt cache: %v", err)
	}
	if client == nil || selected.ID != machine.ID || target.Port != 2222 || target.OSUser != "root" {
		t.Fatalf("resolution = client:%t machine:%+v target:%+v", client != nil, selected, target)
	}
	if machineListCalls.Load() != 0 || sshTargetCalls.Load() != 1 {
		t.Fatalf("live fallback calls: machine list=%d SSH target=%d", machineListCalls.Load(), sshTargetCalls.Load())
	}
	if len(observed) != 1 {
		t.Fatalf("observations = %d, want one cache degradation", len(observed))
	}
	fault := observed[0]
	if fault.Component != "paperboat-cli" || fault.Operation != "ssh" || fault.Stage != "command" || fault.Code != "managed_ssh_failed" || fault.SupportReference != reference {
		t.Fatalf("fault metadata = %+v", fault)
	}
	if strings.Contains(strings.Join(fault.ErrorChain, ","), "CACHE_PRIVATE_SENTINEL") {
		t.Fatal("cache payload reached diagnostics")
	}
	if repaired, ok, err := sshTargetCacheLookup(cfg, machine, time.Now()); err != nil || !ok || repaired.Port != target.Port {
		t.Fatalf("repaired cache = %+v, %t, %v", repaired, ok, err)
	}
	if _, _, cached, err := resolveSSHCommandTargetFast(commandContext, "machine-ready"); err != nil || cached.Port != target.Port {
		t.Fatalf("recovered cache resolution = %+v, %v", cached, err)
	}
	if sshTargetCalls.Load() != 1 || len(observed) != 1 {
		t.Fatalf("recovery repeated live fetch/diagnostic: SSH target calls=%d observations=%d", sshTargetCalls.Load(), len(observed))
	}

	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, target, err := resolveSSHCommandTargetFast(commandContext, "machine-ready"); err != nil || target.Port != 2222 {
		t.Fatalf("resolution with unreadable cache path = %+v, %v", target, err)
	}
	if sshTargetCalls.Load() != 2 || len(observed) != 2 || observed[1].Errno == 0 {
		t.Fatalf("cache-path observation/fallback = calls:%d faults:%+v", sshTargetCalls.Load(), observed)
	}
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	if _, _, target, err := resolveSSHCommandTargetFast(commandContext, "machine-ready"); err != nil || target.Port != 2222 {
		t.Fatalf("resolution after cache-path repair = %+v, %v", target, err)
	}
	if _, ok, err := sshTargetCacheLookup(cfg, machine, time.Now()); err != nil || !ok {
		t.Fatalf("cache was not rewritten after filesystem recovery: hit=%t err=%v", ok, err)
	}
	if len(observed) != 2 {
		t.Fatalf("healthy recovery added cache diagnostics: %+v", observed)
	}
}

func TestSSHTargetCacheRejectsIncompleteEntry(t *testing.T) {
	isolateSSHTargetCache(t)
	cfg := &config.Config{ServerURL: "https://api.paperboat.test"}
	machine := testSSHTargetCacheMachine()
	if err := sshTargetCacheStore(cfg, machine, api.ManagedSSHTarget{OSUser: "root"}, time.Now()); err == nil {
		t.Fatal("entry without a port was accepted")
	}
	if err := sshTargetCacheStore(cfg, machine, api.ManagedSSHTarget{Port: 22}, time.Now()); err == nil {
		t.Fatal("entry without an OS user was accepted")
	}
}
