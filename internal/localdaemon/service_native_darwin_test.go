//go:build darwin

package localdaemon

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestNativeDarwinInstalledCandidateLifecycle(t *testing.T) {
	if os.Getenv("PAPERBOAT_NATIVE_SERVICE_TEST") != "1" {
		t.Skip("set PAPERBOAT_NATIVE_SERVICE_TEST=1 on an isolated macOS account")
	}
	candidate := filepath.Clean(os.Getenv("PAPERBOAT_NATIVE_SERVICE_CANDIDATE"))
	if !filepath.IsAbs(candidate) {
		t.Fatal("PAPERBOAT_NATIVE_SERVICE_CANDIDATE must be absolute")
	}
	expectedVersion := strings.TrimSpace(os.Getenv("PAPERBOAT_NATIVE_SERVICE_VERSION"))
	if expectedVersion == "" {
		t.Fatal("PAPERBOAT_NATIVE_SERVICE_VERSION must name the candidate version")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"config", "state", "run", "identity"} {
		if err := os.MkdirAll(filepath.Join(home, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(home, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = "https://control.invalid"
	cfg.Auth.AllowFileFallback = true
	cfg.Auth.ProfileDir = filepath.Join(home, "credentials")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	identityRoot := filepath.Join(home, "Library", "Application Support", "Paperboat", "runtime")
	store, err := identity.Open(identity.Config{StateRoot: identityRoot})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{
		ServerURL: cfg.ServerURL, MachineID: "machine_task22", EnvironmentID: "environment_task22",
		PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath: filepath.Join(home, "inbox"), InstallationGeneration: 1, SetupMode: "client", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	t.Setenv("TMPDIR", filepath.Join(home, "run"))
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", identityRoot)
	t.Setenv("PAPERBOAT_CONFIG", cfgPath)
	definition := filepath.Join(home, "Library", "LaunchAgents", hostservice.DaemonLabel+".plist")
	if _, err := os.Lstat(definition); err == nil {
		t.Fatalf("refusing to replace existing service definition %s", definition)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	installed := false
	t.Cleanup(func() {
		if installed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cleanupCancel()
			_ = RemoveCurrentUserService(cleanupCtx, candidate)
		}
	})
	if err := InstallCurrentUserService(ctx, candidate, cfgPath, cfg.ServerURL); err != nil {
		t.Fatalf("install: %v", err)
	}
	installed = true
	assertNativeDarwinCandidateReady(t, candidate, expectedVersion)
	info, err := os.Stat(definition)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("definition permissions=%v err=%v", info, err)
	}
	body, err := os.ReadFile(definition)
	if err != nil || !strings.Contains(string(body), "<string>daemon</string>") || !strings.Contains(string(body), candidate) {
		t.Fatalf("definition does not invoke candidate pb daemon: %v", err)
	}

	if err := InstallCurrentUserService(ctx, candidate, cfgPath, cfg.ServerURL); err != nil {
		t.Fatalf("repair: %v", err)
	}
	assertNativeDarwinCandidateReady(t, candidate, expectedVersion)
	domain := "gui/" + strconv.Itoa(os.Getuid()) + "/" + hostservice.DaemonLabel
	if output, err := exec.Command("launchctl", "kill", "SIGKILL", domain).CombinedOutput(); err != nil {
		t.Fatalf("kill service: %v: %s", err, output)
	}
	assertNativeDarwinCandidateReady(t, candidate, expectedVersion)
	if err := RemoveCurrentUserService(ctx, candidate); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	installed = false
	if _, err := os.Lstat(definition); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("definition remains after uninstall: %v", err)
	}
}

func assertNativeDarwinCandidateReady(t *testing.T, candidate, expectedVersion string) {
	t.Helper()
	paths, err := localapi.CurrentPaths(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		snapshot, snapshotErr := client.Snapshot(ctx)
		cancel()
		if snapshotErr == nil && snapshot.DaemonState != "" {
			if snapshot.DaemonVersion != expectedVersion {
				t.Fatalf("authenticated daemon version=%q want candidate version %q", snapshot.DaemonVersion, expectedVersion)
			}
			commandCtx, commandCancel := context.WithTimeout(context.Background(), 3*time.Second)
			output, commandErr := exec.CommandContext(commandCtx, candidate, "status", "--json").CombinedOutput()
			commandCancel()
			if commandErr != nil || len(output) == 0 {
				t.Fatalf("authenticated CLI status failed: %v: %s", commandErr, output)
			}
			return
		}
		if time.Now().After(deadline) {
			domain := "gui/" + strconv.Itoa(os.Getuid()) + "/" + hostservice.DaemonLabel
			output, _ := exec.Command("launchctl", "print", domain).CombinedOutput()
			t.Fatalf("candidate daemon did not become ready: %v; launchd: %s", snapshotErr, output)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
