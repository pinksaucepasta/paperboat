package auth

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

type refreshWriteFailureStore struct {
	config.SecretStore
	failSuffix string
	failPrefix string
}

func (s *refreshWriteFailureStore) Set(ref, value string) error {
	if s.failPrefix != "" && strings.HasPrefix(ref, s.failPrefix) {
		s.failPrefix = ""
		return errors.New("credential storage unavailable")
	}
	if s.failSuffix != "" && strings.HasSuffix(ref, s.failSuffix) {
		s.failSuffix = ""
		return errors.New("credential storage unavailable")
	}
	return s.SecretStore.Set(ref, value)
}

// A fresh source/store must resume the exact durable HTTP attempt, even when
// an earlier process received no response or only partially stored the pair.
func TestRefreshRecoveryResumesOriginalAttemptAfterInterruption(t *testing.T) {
	for _, failure := range []string{"attempt_write", "lost_response", "refresh_write", "access_write", "expired_access"} {
		t.Run(failure, func(t *testing.T) {
			var attempt string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					AttemptID string `json:"attempt_id"`
				}
				if json.NewDecoder(r.Body).Decode(&input) != nil {
					t.Error("missing attempt input")
					http.Error(w, "invalid", 400)
					return
				}
				decoded, err := hex.DecodeString(input.AttemptID)
				if err != nil || len(decoded) != 32 {
					t.Error("invalid attempt identity")
					http.Error(w, "invalid", 400)
					return
				}
				calls++
				if calls == 1 {
					attempt = input.AttemptID
				}
				wantBearer := "Bearer refresh-old"
				if failure == "expired_access" && calls == 3 {
					wantBearer = "Bearer refresh-new"
					if input.AttemptID == attempt {
						t.Error("new rotation reused consumed attempt")
					}
				} else if input.AttemptID != attempt {
					t.Error("interrupted rotation changed attempt")
				}
				if r.Header.Get("Authorization") != wantBearer {
					t.Error("interrupted rotation lost original bearer")
				}
				if calls == 1 && (failure == "lost_response" || failure == "expired_access") {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				expires, access, refresh := 900, "access-new", "refresh-new"
				if failure == "expired_access" && calls == 2 {
					expires = 0
				}
				if failure == "expired_access" && calls == 3 {
					access, refresh = "access-final", "refresh-final"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": expires, "cli_client_session_id": "cls_recovery"}})
			}))
			defer server.Close()
			root := t.TempDir()
			secrets := &refreshWriteFailureStore{SecretStore: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}
			store := config.ProfileStore{Path: root, Secrets: secrets}
			expired := time.Now().Add(-time.Minute)
			if err := store.Save(config.Profile{Issuer: server.URL, CLIClientSessionID: "cls_recovery", AccessExpiresAt: expired}, config.Credential{AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: expired}); err != nil {
				t.Fatal(err)
			}
			if failure == "refresh_write" {
				secrets.failSuffix = "-refresh"
			}
			if failure == "attempt_write" {
				secrets.failPrefix = "refresh-attempt-"
			}
			if failure == "access_write" {
				secrets.failSuffix = "-access"
			}
			if _, err := (&Source{Store: store, Issuer: server.URL}).Credential(); err == nil {
				t.Fatal("interruption unexpectedly succeeded")
			}
			// New owner object, same protected storage: no in-memory request state.
			resumedStore := config.ProfileStore{Path: root, Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}
			credential, err := (&Source{Store: resumedStore, Issuer: server.URL}).Credential()
			if err != nil {
				t.Fatal(err)
			}
			wantAccess, wantCalls := "access-new", 2
			if failure == "attempt_write" {
				wantCalls = 1
			}
			if failure == "expired_access" {
				wantAccess, wantCalls = "access-final", 3
			}
			if credential.AccessToken != wantAccess || calls != wantCalls || !time.Now().Before(credential.ExpiresAt) {
				t.Fatalf("recovery failed: calls=%d expiry=%s", calls, credential.ExpiresAt)
			}
			entries, err := os.ReadDir(filepath.Join(root, "profiles"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatal("refresh attempt metadata not cleaned")
			}
			entries, err = os.ReadDir(filepath.Join(root, "secrets"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatal("original bearer custody not cleaned")
			}
		})
	}
}
