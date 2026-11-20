package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sessionauth "github.com/pinksaucepasta/paperboat/cli/internal/auth"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func TestCanceledOperationReleasesOnlyServerAuthorizedCredentials(t *testing.T) {
	var releases atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releases.Add(1)
		var body map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/v1/operation-access-sessions/release" ||
			r.Header.Get("Authorization") != "Bearer fixture-access" || json.NewDecoder(r.Body).Decode(&body) != nil ||
			body["machine_id"] != "machine_target" || body["operation_id"] != "operation_fixture" || body["capability"] != "exec" {
			t.Error("release did not authenticate and name the completed operation")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, perOperation := range []bool{false, true} {
		profileDir := t.TempDir()
		store := config.ProfileStore{Path: profileDir, Secrets: config.FileSecretStore{Dir: filepath.Join(profileDir, "secrets")}}
		expires := time.Now().Add(time.Hour)
		profile := config.Profile{Issuer: backend.URL, CLIClientSessionID: "cli_fixture", ServerAuthorized: perOperation, AccessExpiresAt: expires}
		if err := store.Save(profile, config.Credential{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
		d := &deps{cfg: &config.Config{ServerURL: backend.URL}, auth: &sessionauth.Source{Store: store, Issuer: backend.URL}}
		releaseOperationCredentials(ctx, d, "machine_target", "operation_fixture", "exec")
		if n := releases.Load(); perOperation && n != 1 || !perOperation && n != 0 {
			t.Fatalf("profile with server authorization=%t made %d release requests", perOperation, n)
		}
	}
}
