package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestRuntimeRefreshStopsInvalidGrantUntilCredentialChanges(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_grant","message":"private text"}}`))
	}))
	defer server.Close()
	store, profile := credentialFailureFixture(t, server.URL)
	source := (&Source{Store: store, Issuer: server.URL}).WithContext(context.Background())
	for range 3 {
		credential, err := source.Refresh()
		if err == nil || credential.AccessToken != "" {
			t.Fatal("rejected refresh returned a credential")
		}
	}
	var signedOut *SignedOutError
	_, terminalErr := source.Credential()
	if !errors.As(terminalErr, &signedOut) || terminalErr.Error() != "Paperboat is signed out; sign in again" {
		t.Fatal("invalid grant did not produce safe signed-out state")
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid grant retried %d times", calls.Load())
	}
	expiry := time.Now().Add(time.Hour)
	profile.CLIClientSessionID = "cls_reauthenticated"
	profile.AccessExpiresAt = expiry
	if err := store.Replace(profile, config.Credential{AccessToken: "replacement", RefreshToken: "replacement-refresh", ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}
	credential, err := source.Credential()
	if err != nil || credential.AccessToken != "replacement" {
		t.Fatal("new credential did not resume sign-in")
	}
}

func TestRuntimeRefreshBackoffPreventsImmediateNetworkRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"access_token": "recovered", "refresh_token": "refresh-new", "token_type": "Bearer", "expires_in": 900, "cli_client_session_id": "cls_1"}})
	}))
	defer server.Close()
	store, _ := credentialFailureFixture(t, server.URL)
	source := (&Source{Store: store, Issuer: server.URL}).WithContext(context.Background())
	_, original := source.Refresh()
	for range 3 {
		credential, err := source.Refresh()
		if err == nil || !errors.Is(err, original) || credential.AccessToken != "" {
			t.Fatal("retry lost failure or returned stale credentials")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("transient refresh retried %d times without backoff", calls.Load())
	}
	// Exercise the real runtime deadline, with no test-only clock seam.
	timer := time.NewTimer(15*time.Second + 100*time.Millisecond)
	defer timer.Stop()
	<-timer.C
	credential, err := source.Refresh()
	if err != nil || credential.AccessToken != "recovered" || calls.Load() != 2 {
		t.Fatal("transient refresh did not resume after backoff")
	}
}

func TestRuntimeRefreshProtocolFailureDoesNotSignOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request"}}`))
	}))
	defer server.Close()
	store, _ := credentialFailureFixture(t, server.URL)
	source := (&Source{Store: store, Issuer: server.URL}).WithContext(context.Background())
	_, err := source.Refresh()
	var signedOut *SignedOutError
	if err == nil || errors.As(err, &signedOut) {
		t.Fatal("protocol failure incorrectly became permanent signed out")
	}
}
