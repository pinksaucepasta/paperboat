package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// X-27: access tokens expire on the server's clock (every check uses serverclock.Now), so a refreshed token's expiry is
// computed on it: on a local clock 10 min ahead it was stored 10 min late and the expired token was sent, refused 401.
func TestRefreshedCredentialExpiresOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	serverclock.Observe(time.Now().Add(-10*time.Minute).UTC().Format(http.TimeFormat), time.Now(), time.Now())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"access_token": "fresh", "refresh_token": "refresh-new", "token_type": "Bearer", "expires_in": 900, "cli_client_session_id": "cls_1"}})
	}))
	defer server.Close()
	store, _ := credentialFailureFixture(t, server.URL)
	source := (&Source{Store: store, Issuer: server.URL}).WithContext(context.Background())
	credential, err := source.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := credential.ExpiresAt, serverclock.Now().Add(900*time.Second); got.Sub(want).Abs() > 10*time.Second {
		t.Fatalf("expiry %s, want about %s (server clock + 900 s)", got, want)
	}
}
