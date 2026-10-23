package github

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

func TestHTTPRefreshOAuthToken(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"success", `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800,"refresh_token_expires_in":15897600,"scope":""}`, 200, nil},
		{"invalid-refresh", `{"error":"bad_refresh_token","error_description":"private-secret"}`, 200, ErrNotConnected},
		{"provider-outage", `{"error":"private-secret"}`, 503, ErrOAuthRefreshUnavailable},
		{"missing-pair", `{"access_token":"private-secret","expires_in":28800}`, 200, ErrOAuthRefreshUnavailable},
		{"malformed", `not-json-private-secret`, 200, ErrOAuthRefreshUnavailable},
		{"oversized", strings.Repeat("private-secret", 6000), 200, ErrOAuthRefreshUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_secret") != "client-secret" {
					t.Error("incorrect refresh request")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			token, err := (HTTPClient{TokenURL: server.URL}).RefreshOAuthToken(t.Context(), OAuthRefreshInput{RefreshToken: "old-refresh", ClientID: "client", ClientSecret: "client-secret"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("provider secret escaped")
			}
			if err == nil && (token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" || time.Until(token.ExpiresAt) < 7*time.Hour || token.RefreshExpiresAt.IsZero()) {
				t.Fatal("invalid rotated pair")
			}
		})
	}
}

type refreshClient struct {
	FakeClient
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
	token   OAuthToken
}

func (c *refreshClient) RefreshOAuthToken(ctx context.Context, in OAuthRefreshInput) (OAuthToken, error) {
	c.calls.Add(1)
	if in.RefreshToken != "old-refresh" {
		return OAuthToken{}, errors.New("unexpected credential")
	}
	if c.started != nil {
		close(c.started)
		select {
		case <-c.release:
		case <-ctx.Done():
			return OAuthToken{}, ctx.Err()
		}
	}
	return c.token, c.err
}

func TestGitHubTokenRefreshPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"concurrent", "revoke", "reconnect", "provider-failure", "invalid-refresh", "malformed", "no-refresh", "refresh-expired"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			id := fmt.Sprintf("usr_gh_refresh_%d", time.Now().UnixNano())
			_, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.com")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, e := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.github_oauth_tokens WHERE user_id=$1`, id)
				if e != nil {
					t.Error(e)
				}
				_, e = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, id)
				if e != nil {
					t.Error(e)
				}
			})
			ac, _ := secrets.Encrypt("refresh-test-key", "old-access")
			rc, _ := secrets.Encrypt("refresh-test-key", "old-refresh")
			if name == "no-refresh" {
				rc = nil
			}
			_, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.github_oauth_tokens(user_id,token_ciphertext,refresh_token_ciphertext,scopes,expires_at,provider_account_login) VALUES($1,$2,$3,ARRAY[]::text[],now()-interval '1 hour','qa')`, id, ac, rc)
			if err != nil {
				t.Fatal(err)
			}
			if name == "refresh-expired" {
				if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.github_oauth_tokens SET refresh_expires_at=now()-interval '1 hour' WHERE user_id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			client := &refreshClient{token: OAuthToken{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(8 * time.Hour)}}
			cfg := config.Config{}
			cfg.Secrets.EncryptionKey = "refresh-test-key"
			cfg.GitHub.AppID = "qa-app"
			service := NewService(store, audit.NewWriter(store), client, cfg)
			if name == "provider-failure" {
				client.err = errors.New("private-secret")
			}
			if name == "invalid-refresh" {
				client.err = ErrNotConnected
			}
			if name == "malformed" {
				client.token.RefreshToken = ""
			}
			if name == "concurrent" {
				var wg sync.WaitGroup
				for range 10 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						token, _, _, e := service.githubTokenWithExpiry(ctx, id)
						if e != nil || token != "new-access" {
							t.Error("concurrent rotation failed")
						}
					}()
				}
				wg.Wait()
				if client.calls.Load() != 1 {
					t.Fatalf("refresh calls=%d", client.calls.Load())
				}
			} else if name == "revoke" || name == "reconnect" {
				client.started = make(chan struct{})
				client.release = make(chan struct{})
				done := make(chan error, 1)
				go func() { _, _, _, e := service.githubTokenWithExpiry(ctx, id); done <- e }()
				select {
				case <-client.started:
				case <-ctx.Done():
					t.Fatal("refresh did not start")
				}
				mutated := make(chan error, 1)
				go func() {
					var e error
					if name == "revoke" {
						_, e = store.SQL().ExecContext(ctx, `UPDATE paperboat.github_oauth_tokens SET revoked_at=now() WHERE user_id=$1`, id)
					} else {
						cipher, _ := secrets.Encrypt("refresh-test-key", "reconnected-access")
						_, e = store.SQL().ExecContext(ctx, `UPDATE paperboat.github_oauth_tokens SET token_ciphertext=$2,expires_at=now()+interval '8 hours',version=version+1 WHERE user_id=$1`, id, cipher)
					}
					mutated <- e
				}()
				close(client.release)
				if e := <-done; e != nil {
					t.Fatal(e)
				}
				if e := <-mutated; e != nil {
					t.Fatal(e)
				}
				token, _, _, e := service.githubTokenWithExpiry(ctx, id)
				if name == "revoke" && !errors.Is(e, ErrNotConnected) {
					t.Fatal("revocation resurrected")
				}
				if name == "reconnect" && (e != nil || token != "reconnected-access") {
					t.Fatal("reconnect overwritten")
				}
			} else {
				_, _, _, e := service.githubTokenWithExpiry(ctx, id)
				want := ErrOAuthRefreshUnavailable
				if name == "invalid-refresh" || name == "no-refresh" || name == "refresh-expired" {
					want = ErrNotConnected
				}
				if !errors.Is(e, want) {
					t.Fatalf("error=%v want=%v", e, want)
				}
				var got []byte
				var expiry sql.NullTime
				if e = store.SQL().QueryRowContext(ctx, `SELECT token_ciphertext,expires_at FROM paperboat.github_oauth_tokens WHERE user_id=$1`, id).Scan(&got, &expiry); e != nil {
					t.Fatal(e)
				}
				token, _ := secrets.Decrypt("refresh-test-key", got)
				if token != "old-access" || !expiry.Time.Before(time.Now()) {
					t.Fatal("failure changed credential")
				}
				if name == "provider-failure" {
					client.err = nil
					if token, _, _, e = service.githubTokenWithExpiry(ctx, id); e != nil || token != "new-access" {
						t.Fatal("provider recovery failed")
					}
				}
				if name == "no-refresh" || name == "invalid-refresh" || name == "refresh-expired" {
					status, e := service.Status(ctx, id)
					if e != nil || status.Connected {
						t.Fatal("expired credential reported connected")
					}
				}
			}
		})
	}
}

func TestHTTPRefreshRejectsRedirectAndCancellation(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := HTTPClient{TokenURL: redirect.URL}
	if _, err := client.RefreshOAuthToken(t.Context(), OAuthRefreshInput{RefreshToken: "private-refresh"}); !errors.Is(err, ErrOAuthRefreshUnavailable) {
		t.Fatal("redirect accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("refresh credential forwarded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.RefreshOAuthToken(ctx, OAuthRefreshInput{}); !errors.Is(err, ErrOAuthRefreshUnavailable) {
		t.Fatal("cancellation ignored")
	}
}
