//go:build linux || darwin

package daemoncmd

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
				err = store.SaveRegistration(identity.Registration{ServerURL: "https://control.invalid", MachineID: "machine_test", EnvironmentID: "environment_test", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, SetupMode: "client", UpdatedAt: time.Now()})
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
