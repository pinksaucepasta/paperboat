//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

type loginInventorySource func(context.Context) ([]api.UserMachine, error)

func (f loginInventorySource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	return f(ctx)
}

func TestBrowserLoginRefreshesSignedOutDaemonBeforeSuccess(t *testing.T) {
	var backend *httptest.Server
	backend = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			writeAPIData(t, w, map[string]any{"device_code": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "user_code": "ABCD-EFGH", "verification_uri": backend.URL + "/cli/authorize", "verification_uri_complete": backend.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			writeAPIData(t, w, map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "token_type": "Bearer", "expires_in": 3600, "cli_client_session_id": "cls_inventory"})
		case "/v1/me":
			writeAPIData(t, w, map[string]string{"id": "usr_browser", "email": "browser@example.test", "status": "active"})
		case "/v1/e2ee/bootstrap":
			acceptBrowserPeerEnrollment(t, w, r, "usr_browser", "cls_inventory")
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	oldHTTP := loginHTTPClient
	loginHTTPClient = backend.Client
	t.Cleanup(func() { loginHTTPClient = oldHTTP })
	configPath, profiles := browserLoginFixture(t, backend.URL)
	root := commandRuntimeTestRoot(t)
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(root, "identity"))
	state, err := runtimeIdentityStore()
	if err != nil {
		t.Fatal(err)
	}
	key := state.Current()
	if err = state.SaveRegistration(identity.Registration{ServerURL: backend.URL, MachineID: "machine_login", EnvironmentID: "machine_login", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: root, InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	paths := localapi.Paths{StateRoot: root, RuntimeRoot: root, SocketPath: filepath.Join(root, "api.sock"), LockPath: filepath.Join(root, "daemon.lock")}
	oldPaths := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) { return paths, nil }
	t.Cleanup(func() { currentLocalDaemonPaths = oldPaths })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- localdaemon.Run(ctx, localdaemon.DaemonConfig{Paths: paths, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), RefreshInterval: time.Hour, Source: loginInventorySource(func(context.Context) ([]api.UserMachine, error) { _, e := profiles.Load(backend.URL); return nil, e })})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	waitForCommandSocket(t, paths.SocketPath)
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, e := client.Snapshot(ctx)
		if e == nil && snapshot.DaemonState == "awaiting_enrollment" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("signed-out daemon not ready: %v", e)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = runBrowserLogin(t, ctx, configPath, "login", "--no-browser", "--json"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil || snapshot.DaemonState != "ready" {
		t.Fatalf("login returned before inventory activation: state=%s err=%v", snapshot.DaemonState, err)
	}
}
