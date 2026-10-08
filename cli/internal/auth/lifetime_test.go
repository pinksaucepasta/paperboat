package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestRuntimeLifetimeCancelsCredentialRefresh(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	dir := t.TempDir()
	store := config.ProfileStore{Path: dir, Secrets: config.FileSecretStore{Dir: filepath.Join(dir, "secrets")}}
	expired := time.Now().Add(-time.Minute)
	if err := store.Save(config.Profile{Issuer: server.URL, CLIClientSessionID: "cls_test", AccessExpiresAt: expired}, config.Credential{AccessToken: "access_test", RefreshToken: "refresh_test", ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := (&Source{Store: store, Issuer: server.URL}).WithContext(ctx)
	done := make(chan error, 1)
	go func() { _, err := source.Credential(); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh survived runtime cancellation")
	}
}
