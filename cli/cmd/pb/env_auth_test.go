package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestEnvironmentClientRefreshesExpiredSession(t *testing.T) {
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
	flags := flag.NewFlagSet("env", flag.ContinueOnError)
	flags.String("config", path, "")
	flags.String("server", server.URL, "")
	client, _, _, err := e2eeClient(command.NewContext(flags))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetPasswordVault(context.Background()); !api.IsNotFound(err) {
		t.Fatal(err)
	}
	if refreshed != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshed)
	}
}
