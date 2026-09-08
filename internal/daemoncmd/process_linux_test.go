//go:build linux

package daemoncmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// Run against the candidate executable, built once in the ignored build directory.
// This tests local authentication and process recovery without deploying a backend.
func TestLinuxExecutableLifecycle(t *testing.T) {
	bin := os.Getenv("PAPERBOAT_TEST_BIN_DIR")
	if bin == "" {
		t.Skip("set PAPERBOAT_TEST_BIN_DIR to candidate pb directory")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "pb21-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, name := range []string{"home", "config", "state", "run", "identity"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(root, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = "https://control.invalid"
	cfg.Auth.AllowFileFallback = true
	cfg.Auth.ProfileDir = filepath.Join(root, "credentials")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := identity.Open(identity.Config{StateRoot: filepath.Join(root, "identity")})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: cfg.ServerURL, MachineID: "machine_test", EnvironmentID: "environment_test", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, SetupMode: "client", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+filepath.Join(root, "home"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_RUNTIME_DIR="+filepath.Join(root, "run"), "PAPERBOAT_RUNTIME_STATE_ROOT="+filepath.Join(root, "identity"), "PAPERBOAT_CONFIG="+cfgPath)
	socket := filepath.Join(root, "run", "paperboat", "local-api.sock")
	client, err := localapi.NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := func() (*exec.Cmd, <-chan error) {
		t.Helper()
		cmd := exec.Command(filepath.Join(bin, "pb"), "daemon", "--config", cfgPath)
		cmd.Env = env
		// Production diagnostics may contain operational data; don't copy them into test output.
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		reaped := make(chan struct{})
		go func() { done <- cmd.Wait(); close(reaped) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if snapshot, err := client.Snapshot(context.Background()); err == nil && snapshot.DaemonState == "degraded" {
				return cmd, done
			}
			select {
			case err := <-done:
				t.Fatalf("daemon exited before IPC readiness: %v", err)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("daemon did not expose authenticated IPC")
		return nil, nil
	}
	status := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, "pb"), "status", "--json")
		cmd.Env = env
		data, err := cmd.Output()
		if err != nil {
			t.Fatalf("real CLI status failed: %v", err)
		}
		var snapshot localapi.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
		// Missing control-plane credentials must be surfaced, never falsely declared ready.
		if snapshot.DaemonState != "degraded" {
			t.Fatalf("uncredentialed fixture state = %q", snapshot.DaemonState)
		}
	}
	wait := func(done <-chan error, killed bool) {
		t.Helper()
		select {
		case err := <-done:
			if !killed && err != nil {
				t.Fatalf("graceful exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not drain and stop")
		}
	}
	first, done := start()
	status()
	duplicateCtx, cancelDuplicate := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDuplicate()
	duplicate := exec.CommandContext(duplicateCtx, filepath.Join(bin, "pb"), "daemon", "--config", cfgPath)
	duplicate.Env = env
	if err := duplicate.Run(); err == nil || duplicateCtx.Err() != nil {
		t.Fatal("second daemon acquired the same state")
	}
	status()
	// Keep a real authenticated streaming request open across graceful draining.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	updates, watchErrors := client.Watch(watchCtx, 0)
	select {
	case <-updates:
	case err := <-watchErrors:
		t.Fatalf("watch: %v", err)
	case <-time.After(time.Second):
		t.Fatal("watch did not connect")
	}
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait(done, false)
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("graceful stop left socket: %v", err)
	}
	select {
	case <-watchErrors:
	case <-time.After(2 * time.Second):
		t.Fatal("watch survived daemon drain")
	}
	second, done := start()
	status()
	if err := second.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	wait(done, true)
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("crash fixture did not leave stale socket: %v", err)
	}
	third, done := start()
	status()
	// Verify OS credentials as well as mode bits: allow a foreign UID to reach this
	// task-owned socket, then require an HTTP denial before any API data is returned.
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		t.Fatal("multi-user check requires passwordless sudo")
	}
	for _, dir := range []string{root, filepath.Join(root, "run"), filepath.Dir(socket)} {
		if err := os.Chmod(dir, 0711); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(socket, 0666); err != nil {
		t.Fatal(err)
	}
	foreign := exec.Command("sudo", "-n", "-u", "nobody", "curl", "--silent", "--show-error", "--max-time", "2", "--unix-socket", socket, "--write-out", "\n%{http_code}", "http://paperboat.local/v1/status")
	output, err := foreign.Output()
	if err != nil {
		t.Fatalf("foreign UID request: %v", err)
	}
	if !bytes.Contains(output, []byte("permission_denied")) || !strings.HasSuffix(string(output), "\n403") {
		t.Fatal("foreign UID was not denied by IPC authorizer")
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	if err := third.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait(done, false)
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final socket cleanup: %v", err)
	}
	t.Log(fmt.Sprintf("candidate executable: %s; status, exclusive owner, drain, crash/restart and foreign-UID denial passed", bin))
}
