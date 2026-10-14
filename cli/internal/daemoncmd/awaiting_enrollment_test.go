//go:build linux || darwin

package daemoncmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestAwaitingEnrollmentServesIPCAndTransitions(t *testing.T) {
	for _, enroll := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "enroll"}[enroll], func(t *testing.T) {
			root, err := os.MkdirTemp("", "pb-enroll-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "identity"))
			cfg, err := config.Load(filepath.Join(root, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			paths := localapi.Paths{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), SocketPath: filepath.Join(root, "run", "api.sock"), LockPath: filepath.Join(root, "state", "daemon.lock")}
			for _, dir := range []string{paths.StateRoot, paths.RuntimeRoot} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runAwaitingEnrollment(ctx, cfg, paths) }()
			client, err := localapi.NewClient(paths.SocketPath, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			ready := false
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				s, err := client.Snapshot(ctx)
				if err == nil && s.DaemonState == "awaiting_enrollment" {
					ready = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !ready {
				cancel()
				t.Fatalf("un-enrolled daemon IPC not ready: %v", <-done)
			}
			if enroll {
				store, err := runtimeIdentityStore()
				if err != nil {
					t.Fatal(err)
				}
				key := store.Current()
				err = store.SaveRegistration(identity.Registration{ServerURL: "https://control.invalid", MachineID: "machine_test", EnvironmentID: "environment_test", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if enroll && err != nil || !enroll && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("enrollment watcher did not stop")
			}
			if _, err := os.Stat(paths.SocketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("local socket leaked", err)
			}
		})
	}
}

func TestAwaitingEnrollmentCancellationRetainsStartupFailure(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "PRIVATE_ENROLLMENT_STATE")
	if err := os.WriteFile(state, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := localapi.Paths{StateRoot: state, RuntimeRoot: filepath.Join(root, "run"), SocketPath: filepath.Join(root, "run", "api.sock"), LockPath: filepath.Join(state, "daemon.lock")}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = runAwaitingEnrollment(ctx, cfg, paths)
	var native *os.PathError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &native) {
		t.Fatal("caller cancellation hid local IPC startup failure", err)
	}
}

func TestDaemonReportsCredentialProfileFailureBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	oldConfigHome := xdg.ConfigHome
	xdg.ConfigHome = filepath.Join(root, "config-home")
	t.Cleanup(func() { xdg.ConfigHome = oldConfigHome })
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "identity"))
	cfg, err := config.Load(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.ProfileDir = filepath.Join(root, "PRIVATE_PROFILE_STORE")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Path, "profiles"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	identityStore, err := runtimeIdentityStore()
	if err != nil {
		t.Fatal(err)
	}
	key := identityStore.Current()
	if err := identityStore.SaveRegistration(identity.Registration{ServerURL: cfg.ServerURL, MachineID: "machine_test", EnvironmentID: "environment_test", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	oldPaths := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) {
		return localapi.Paths{StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"), SocketPath: filepath.Join(root, "run", "api.sock"), LockPath: filepath.Join(root, "state", "lock")}, nil
	}
	t.Cleanup(func() { currentLocalDaemonPaths = oldPaths })
	command := NewCommand()
	command.SetArgs([]string{"--config", cfg.Path()})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = command.ExecuteContext(ctx)
	var native *os.PathError
	if !errors.As(err, &native) || native.Op != "open" || strings.Contains(err.Error(), "PRIVATE_PROFILE_STORE") {
		t.Fatal("daemon hid or exposed its credential-profile failure", err)
	}
}
