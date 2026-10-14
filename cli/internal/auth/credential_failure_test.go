package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

type privateCredentialCause struct{}

func (*privateCredentialCause) Error() string { panic("credential cause must not be formatted") }

type faultAccountSecrets struct {
	config.SecretStore
	ref     string
	failure error
}

func (s *faultAccountSecrets) Get(ref string) (string, error) {
	if ref == s.ref && s.failure != nil {
		return "", s.failure
	}
	return s.SecretStore.Get(ref)
}

func credentialFailureFixture(t *testing.T, issuer string) (config.ProfileStore, config.Profile) {
	t.Helper()
	dir := t.TempDir()
	store := config.ProfileStore{Path: dir, Secrets: config.FileSecretStore{Dir: filepath.Join(dir, "secrets")}}
	expiry := time.Now().Add(time.Hour)
	if err := store.Save(config.Profile{Issuer: issuer, CLIClientSessionID: "cls_1", AccessExpiresAt: expiry}, config.Credential{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: expiry}); err != nil {
		t.Fatal("credential fixture creation failed")
	}
	profile, err := store.Load(issuer)
	if err != nil {
		t.Fatal("credential fixture load failed")
	}
	return store, profile
}

func TestAccountCredentialFailureDistinguishesMissingTokensAndRecovers(t *testing.T) {
	for _, which := range []string{"access", "refresh"} {
		t.Run(which, func(t *testing.T) {
			store, profile := credentialFailureFixture(t, "https://api.example.test")
			ref, value := profile.AccessSecretRef, "access-old"
			if which == "refresh" {
				ref, value = profile.RefreshSecretRef, "refresh-old"
			}
			if err := store.Secrets.Delete(ref); err != nil {
				t.Fatal("owned token deletion failed")
			}
			source := &Source{Store: store, Issuer: profile.Issuer}
			credential, err := source.Credential()
			owned, ok := err.(*CredentialFailure)
			if !ok || owned == nil || !errors.Is(err, config.ErrSecretNotFound) || credential.AccessToken != "" || credential.RefreshToken != "" {
				t.Fatal("missing account token lost purpose/cause or returned credentials")
			}
			if owned.Error() != "Paperboat account credentials are unavailable" {
				t.Fatal("account failure exposed nonstatic text")
			}
			if err := store.Secrets.Set(ref, value); err != nil {
				t.Fatal("owned token replacement failed")
			}
			credential, err = source.Credential()
			if err != nil || credential.AccessToken != "access-old" || credential.RefreshToken != "refresh-old" {
				t.Fatal("replacement account credential did not recover")
			}
		})
	}
}

func TestAccountCredentialFailurePreservesMixedIOWithoutFormatting(t *testing.T) {
	store, profile := credentialFailureFixture(t, "https://api.example.test")
	private := &privateCredentialCause{}
	secrets := &faultAccountSecrets{SecretStore: store.Secrets, ref: profile.AccessSecretRef, failure: errors.Join(config.ErrSecretNotFound, syscall.EIO, private)}
	store.Secrets = secrets
	source := &Source{Store: store, Issuer: profile.Issuer}
	credential, err := source.Credential()
	owned, ok := err.(*CredentialFailure)
	if !ok || owned == nil || !errors.Is(err, config.ErrSecretNotFound) || !errors.Is(err, syscall.EIO) || !errors.Is(err, private) || credential.AccessToken != "" {
		t.Fatal("mixed account failure lost an independent cause")
	}
	if err.Error() != "Paperboat account credentials are unavailable" {
		t.Fatal("private account failure formatted")
	}
	if accountCredentialFailure(owned) != owned {
		t.Fatal("existing purpose owner wrapped twice")
	}
	secrets.failure = nil
	if recovered, err := source.Credential(); err != nil || recovered.AccessToken != "access-old" {
		t.Fatal("healthy secret store did not recover")
	}
}

func TestAccountCredentialFailureHTTPRefreshFailureAndRecovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/refresh" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"temporarily_unavailable","message":"private provider text"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"access_token": "access-new", "refresh_token": "refresh-new", "token_type": "Bearer", "expires_in": 900, "cli_client_session_id": "cls_1"}})
	}))
	defer server.Close()
	store, profile := credentialFailureFixture(t, server.URL)
	source := &Source{Store: store, Issuer: profile.Issuer}
	credential, err := source.Refresh()
	var apiFailure *api.APIError
	if _, ok := err.(*CredentialFailure); !ok || !errors.As(err, &apiFailure) || apiFailure.Status != http.StatusServiceUnavailable || credential.AccessToken != "" {
		t.Fatal("actual refresh failure lost typed status/cause")
	}
	preserved, err := store.CredentialFor(profile.Issuer)
	if err != nil || preserved.AccessToken != "access-old" || preserved.RefreshToken != "refresh-old" {
		t.Fatal("failed refresh erased usable saved credentials")
	}
	credential, err = source.Refresh()
	if err != nil || credential.AccessToken != "access-new" || credential.RefreshToken != "refresh-new" || calls.Load() != 2 {
		t.Fatal("fresh refresh did not recover exactly once")
	}
}
