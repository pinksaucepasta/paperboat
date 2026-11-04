package paperboat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/client/api"
	"github.com/pinksaucepasta/paperboat/shared/client/state"
)

type sdkServer struct {
	mu           sync.Mutex
	server       *httptest.Server
	starts       int
	revokes      int
	revokeStatus int
	revoked      map[string]bool
	inventory    func(http.ResponseWriter, *http.Request)
	revoke       func(http.ResponseWriter, *http.Request)
}

func newSDKServer(t *testing.T) *sdkServer {
	t.Helper()
	f := &sdkServer{revoked: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		switch r.URL.Path {
		case "/v1/auth/api-token-sessions":
			if r.Header.Get("Authorization") != "Bearer api-test-secret" {
				f.mu.Unlock()
				t.Error("API token exchange used another identity")
				w.WriteHeader(401)
				return
			}
			f.starts++
			id := fmt.Sprintf("session-%d", f.starts)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TokenSet{AccessToken: "access-" + id, RefreshToken: "refresh-" + id, TokenType: "Bearer", ExpiresIn: 3600, CLIClientSessionID: id}})
			return
		case "/v1/auth/token/revoke":
			f.revokes++
			status, hook := f.revokeStatus, f.revoke
			if status == 0 {
				f.revoked[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer refresh-")] = true
			}
			f.mu.Unlock()
			if hook != nil {
				hook(w, r)
				return
			}
			if status != 0 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"revoke_unavailable"}}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case "/v1/machines":
			id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access-")
			allowed := strings.HasPrefix(id, "session-") && !f.revoked[id]
			hook := f.inventory
			f.mu.Unlock()
			if !allowed {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if hook != nil {
				hook(w, r)
				return
			}
			writeInventory(w, r, nil, 0, nil)
			return
		}
		f.mu.Unlock()
		t.Errorf("unexpected SDK route: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *sdkServer) options() Options {
	return Options{ServerURL: f.server.URL, Token: "api-test-secret"}
}
func (f *sdkServer) counts() (int, int) { f.mu.Lock(); defer f.mu.Unlock(); return f.starts, f.revokes }
func writeInventory(w http.ResponseWriter, r *http.Request, items []map[string]any, total int, next *int) {
	if items == nil {
		items = []map[string]any{}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "pagination": map[string]any{"limit": limit, "offset": offset, "total": total, "next_offset": next}}})
}
func newClient(t *testing.T, options Options) *Client {
	t.Helper()
	c, err := New(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSDKClientStateIsolation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	sentinel := filepath.Join(root, "config", "paperboat", "config.json")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"access_token":"cli-state-must-not-be-used"}`)
	if err := os.WriteFile(sentinel, original, 0600); err != nil {
		t.Fatal(err)
	}
	f := newSDKServer(t)
	var mu sync.Mutex
	identities := map[string]bool{}
	f.inventory = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		identities[r.Header.Get("Authorization")] = true
		mu.Unlock()
		writeInventory(w, r, nil, 0, nil)
	}
	a, b := newClient(t, f.options()), newClient(t, f.options())
	if _, err := a.Machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := len(identities)
	mu.Unlock()
	if count != 2 {
		t.Fatal("clients shared renewable credentials")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Machines(context.Background()); err != nil {
		t.Fatal("closing one client affected the other")
	}
	if _, err := a.Machines(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed client accepted work")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if starts, revokes := f.counts(); starts != 2 || revokes != 2 {
		t.Fatalf("sessions=%d revokes=%d", starts, revokes)
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatal("CLI login state changed")
	}
	files := 0
	if err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("memory mode created files: %d", files)
	}
}

func TestSDKStateDirectoryOwnership(t *testing.T) {
	t.Run("lock, permissions, token file, reopen and logout", testSDKDirectoryLifecycle)
	t.Run("revoke failure is retained and retried", testSDKRevokeRecovery)
}
func testSDKDirectoryLifecycle(t *testing.T) {
	f := newSDKServer(t)
	dir := filepath.Join(t.TempDir(), "sdk")
	tokenFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(tokenFile, []byte("api-test-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := Options{ServerURL: f.server.URL, TokenFile: tokenFile, StateDir: dir}
	c := newClient(t, options)
	if _, err := New(context.Background(), options); !errors.Is(err, state.ErrDirectoryBusy) {
		t.Fatalf("directory contention: %v", err)
	}
	sessionPath := filepath.Join(dir, "sdk-session.json")
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("api-test-secret")) {
		t.Fatal("API token persisted")
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{dir: 0700, sessionPath: 0600, filepath.Join(dir, ".sdk.lock"): 0600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatal("state permissions are not owner-only")
			}
		}
	}
	otherOptions := f.options()
	otherOptions.StateDir = t.TempDir()
	otherClient := newClient(t, otherOptions)
	if err := otherClient.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newClient(t, Options{ServerURL: f.server.URL, StateDir: dir})
	if starts, revokes := f.counts(); starts != 2 || revokes != 0 {
		t.Fatalf("reopen started or revoked a session: %d/%d", starts, revokes)
	}
	if _, err = reopened.Machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	checkDamagedSDKState(t, options, sessionPath)
	other := newSDKServer(t)
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = New(context.Background(), Options{ServerURL: other.server.URL, StateDir: dir}); err == nil {
		t.Fatal("foreign issuer state accepted")
	}
	reopened = newClient(t, Options{ServerURL: f.server.URL, StateDir: dir})
	if err = reopened.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logout retained session state")
	}
	if _, err = reopened.Machines(context.Background()); err == nil {
		t.Fatal("logged-out client authorized work")
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := newClient(t, options)
	if starts, _ := f.counts(); starts != 3 {
		t.Fatal("logout session was reused")
	}
	if err = replacement.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sdk-session-") {
			t.Fatal("atomic-write temporary file leaked")
		}
	}
}
func checkDamagedSDKState(t *testing.T, options Options, sessionPath string) {
	t.Helper()
	saved, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"issuer":"partial-state","access_token":"partial"}`)
	if err = os.WriteFile(sessionPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	rejected, rejectErr := New(context.Background(), options)
	if rejectErr == nil {
		_ = rejected.Close()
		t.Fatal("incomplete state was silently replaced")
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil || !bytes.Equal(after, corrupt) {
		t.Fatal("invalid state was overwritten")
	}
	if err = os.WriteFile(sessionPath, saved, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err = os.Chmod(sessionPath, 0644); err != nil {
			t.Fatal(err)
		}
		rejected, rejectErr = New(context.Background(), options)
		if rejectErr == nil {
			_ = rejected.Close()
			t.Fatal("readable-by-others state was accepted")
		}
		if err = os.Chmod(sessionPath, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func testSDKRevokeRecovery(t *testing.T) {
	f := newSDKServer(t)
	options := f.options()
	options.StateDir = t.TempDir()
	c := newClient(t, options)
	f.mu.Lock()
	f.revokeStatus = http.StatusServiceUnavailable
	f.mu.Unlock()
	if err := c.Logout(context.Background()); err == nil {
		t.Fatal("revoke failure hidden")
	}
	if _, err := c.Machines(context.Background()); err == nil {
		t.Fatal("revoking session still authorized work")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), options); err == nil {
		t.Fatal("pending revocation was dropped")
	}
	f.mu.Lock()
	f.revokeStatus = 0
	f.mu.Unlock()
	recovered := newClient(t, options)
	if _, err := recovered.Machines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if starts, revokes := f.counts(); starts != 2 || revokes != 3 {
		t.Fatalf("revocation recovery: starts=%d attempts=%d", starts, revokes)
	}
}

func TestSDKCloseCancelsAndJoins(t *testing.T) {
	f := newSDKServer(t)
	entered := make(chan struct{})
	f.inventory = func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }
	c := newClient(t, f.options())
	result := make(chan error, 1)
	go func() { _, err := c.Machines(context.Background()); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	started := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("close did not cancel active work promptly")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active work error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active work was not joined")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, revokes := f.counts(); revokes != 1 {
		t.Fatalf("repeated Close revoked %d times", revokes)
	}
}

func TestSDKCloseReportsRevokeFailure(t *testing.T) {
	f := newSDKServer(t)
	f.revokeStatus = http.StatusServiceUnavailable
	c := newClient(t, f.options())
	first := c.Close()
	var refusal *api.ResponseError
	if !errors.As(first, &refusal) || refusal.Status != http.StatusServiceUnavailable {
		t.Fatalf("revoke error: %v", first)
	}
	if again := c.Close(); again != first {
		t.Fatal("Close did not retain its result")
	}
	if _, attempts := f.counts(); attempts != 1 {
		t.Fatal("Close retried an unchanged revoke failure")
	}
	if _, err := c.Machines(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal("failed Close accepted work")
	}
}

func TestSDKCloseBoundsStalledRevoke(t *testing.T) {
	f := newSDKServer(t)
	canceled := make(chan struct{})
	f.revoke = func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(canceled) }
	c := newClient(t, f.options())
	start := time.Now()
	first := c.Close()
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("stalled revoke: %v", first)
	}
	if time.Since(start) > closeTimeout+2*time.Second {
		t.Fatal("Close exceeded its shutdown bound")
	}
	if again := c.Close(); again != first {
		t.Fatal("Close changed its result")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("revoke request was not canceled")
	}
}
