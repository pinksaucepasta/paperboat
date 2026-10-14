package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestDesktopAuthUsesDashboardEnrollmentAndRejectsRevokedCredential(t *testing.T) {
	dashboard := "https://dashboard.paperboat.test/dashboard/machines"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/client-configuration":
			_, _ = w.Write([]byte(`{"data":{"version":"1","machines_url":"` + dashboard + `"}}`))
		case "/v1/me":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"Authentication is required."}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = server.URL
	cfg.Auth.ProfileDir = filepath.Join(t.TempDir(), "profiles")
	cfg.Auth.AllowFileFallback = true
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	command := newRootCommand()
	command.SetContext(context.Background())
	if err := command.PersistentFlags().Set("config", path); err != nil {
		t.Fatal(err)
	}

	assertPending := func(value any) {
		t.Helper()
		data, _ := json.Marshal(value)
		var state map[string]any
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		if state["status"] != "pending" || state["verification_uri"] != dashboard || state["message"] != dashboardEnrollmentGuidance {
			t.Fatalf("state = %#v", state)
		}
	}
	value, err := handleDesktop(command, desktopRequest{Action: "auth.login"})
	if err != nil {
		t.Fatal(err)
	}
	assertPending(value)

	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := store.Save(config.Profile{Issuer: server.URL, CLIClientSessionID: "session_revoked", AccessExpiresAt: expires}, config.Credential{AccessToken: "revoked", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	value, err = handleDesktop(command, desktopRequest{Action: "auth.poll"})
	if err != nil {
		t.Fatal(err)
	}
	assertPending(value)
}

func TestDesktopStrictRequestRejectsCommandInjectionFields(t *testing.T) {
	for _, input := range []string{`{"action":"overview","command":"rm"}`, `{"action":"overview"} {}`, `{"action":4}`} {
		var value desktopRequest
		if decodeDesktop([]byte(input), &value) == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
