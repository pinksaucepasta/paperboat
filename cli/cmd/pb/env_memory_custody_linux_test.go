//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

// Exercise the real command factory and authenticated Unix transport with two
// independent CLI clients; no daemon credential-store or password mocks.
func TestHeadlessVaultUnlockSurvivesCLIExitUntilLock(t *testing.T) {
	root := commandRuntimeTestRoot(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_RUNTIME_DIR", root)
	var vault api.PasswordVaultState
	var remoteMu sync.Mutex
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteMu.Lock()
		defer remoteMu.Unlock()
		if r.URL.Path != "/v1/environment-vault" || r.Header.Get("Authorization") != "Bearer access-old" {
			t.Error("unexpected unauthenticated vault request")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodPut {
			var in struct {
				Envelope string `json:"envelope"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil {
				t.Error("invalid vault publication")
				w.WriteHeader(400)
				return
			}
			raw, err := base64.RawURLEncoding.DecodeString(in.Envelope)
			if err != nil {
				t.Error("invalid envelope")
				w.WriteHeader(400)
				return
			}
			head, _, err := environmente2ee.InspectPasswordVault(raw)
			if err != nil {
				t.Error("invalid signed vault")
				w.WriteHeader(400)
				return
			}
			vault = api.PasswordVaultState{Issuer: head.Issuer, AccountID: head.AccountID, Generation: head.Generation, DocumentID: head.ID.String(), Envelope: in.Envelope}
		}
		if vault.DocumentID == "" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": vault})
	}))
	defer control.Close()
	configPath := filepath.Join(root, "config.json")
	_ = writeAuthLoginTestProfile(t, root, configPath, control.URL)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	custody := localapi.NewVaultCustodyStore()
	defer custody.Close()
	server, err := localapi.NewServer(localapi.ServerConfig{SocketPath: paths.SocketPath, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: snapshots, VaultCustody: custody})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("user daemon did not stop")
		}
	})
	waitForCommandSocket(t, paths.SocketPath)
	manager := func(action string) environmentmanager.PasswordVault {
		t.Helper()
		cmd := &cobra.Command{Use: action}
		cmd.SetContext(context.Background())
		cmd.Flags().String("config", configPath, "")
		cmd.Flags().String("server", control.URL, "")
		result, err := passwordVaultForCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	password := []byte("headless private test password")
	defer clear(password)
	a := manager("init")
	if err := a.Initialize(ctx, password); err != nil {
		t.Fatal(err)
	}
	initial, err := a.Store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Clear()
	if err := a.Store.LockPasswordVault(a.Issuer, a.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := manager("unlock").Unlock(ctx, []byte("wrong password")); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatalf("wrong password result=%v", err)
	}
	if err := manager("unlock").Unlock(ctx, password); err != nil {
		t.Fatal(err)
	}
	// A distinct CLI obtains the same keys after the unlock invocation has ended.
	b := manager("list")
	unlocked, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlocked.Clear()
	if !bytes.Equal(unlocked.Payload, initial.Payload) {
		t.Fatal("second CLI did not receive unlocked custody")
	}
	newPassword := []byte("replacement headless test password")
	defer clear(newPassword)
	if err := b.ChangePassword(ctx, newPassword); err != nil {
		t.Fatalf("second CLI could not manage the unlocked vault: %v", err)
	}
	if err := manager("lock").Store.LockPasswordVault(b.Issuer, b.AccountID); err != nil {
		t.Fatal(err)
	}
	locked, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Clear()
	if len(locked.Payload) != 0 || locked.Pending != nil && len(locked.Pending.Payload) != 0 {
		t.Fatal("lock retained plaintext keys")
	}
	if err := manager("password").ChangePassword(ctx, password); !errors.Is(err, environmentmanager.ErrVaultLocked) {
		t.Fatalf("locked management result=%v", err)
	}
	// The volatile backend creates neither an ENV journal nor mutation lock files.
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if entry.Name() == "config.json" || entry.Name() == "credentials-location.json" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, initial.Payload) || bytes.Contains(data, password) {
			t.Error("vault secret persisted to disk")
		}
		if bytes.Contains([]byte(entry.Name()), []byte("environment-mutations")) || bytes.Contains([]byte(entry.Name()), []byte("password-vault")) {
			t.Errorf("volatile custody created disk state: %s", entry.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
