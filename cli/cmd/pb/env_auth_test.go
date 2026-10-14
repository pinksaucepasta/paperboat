package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

func TestEnvironmentVaultUsesSecureCustodyWithFileBackedSession(t *testing.T) {
	refreshed := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/token/refresh":
			if r.Header.Get("Authorization") != "Bearer refresh-old" {
				t.Error("refresh did not use refresh credential")
			}
			refreshed++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"access_token":"access-new","refresh_token":"refresh-new","token_type":"Bearer","expires_in":900,"cli_client_session_id":"cls_old"}}`))
		case "/v1/environment-vault":
			w.Header().Set("Cache-Control", "no-store")
			if r.Header.Get("Authorization") != "Bearer access-new" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found"}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	store := writeAuthLoginTestProfile(t, root, path, server.URL)
	profile, err := store.Load(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Minute)
	profile.AccessExpiresAt = expired
	if err := store.Replace(profile, config.Credential{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("config", path, "")
	cmd.Flags().String("server", server.URL, "")
	manager, err := passwordVaultForCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Client.GetPasswordVault(context.Background()); !api.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := manager.Store.RequireEnvironmentSecureStore(); err != nil {
		t.Fatalf("ENV custody inherited the file-backed login store: %v", err)
	}
	if manager.AccountID != profile.Account.ID || manager.Store.Path != store.Path {
		t.Fatal("ENV custody changed the account or profile location")
	}
	retained, err := store.Load(server.URL)
	if err != nil || retained.CLIClientSessionID != profile.CLIClientSessionID {
		t.Fatalf("existing login session not preserved: %v", err)
	}
	if refreshed != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshed)
	}
}
