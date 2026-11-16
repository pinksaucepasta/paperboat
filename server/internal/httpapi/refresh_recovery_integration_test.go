package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func refreshAttemptRequest(router http.Handler, token, attempt string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"attempt_id": attempt})
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/token/refresh", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func refreshAttemptTokens(t *testing.T, response *httptest.ResponseRecorder) tokenResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("refresh status=%d", response.Code)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("refresh response is cacheable")
	}
	var envelope struct {
		Data tokenResponse `json:"data"`
	}
	if json.Unmarshal(response.Body.Bytes(), &envelope) != nil {
		t.Fatal("refresh response invalid")
	}
	return envelope.Data
}

func TestRefreshRecoveryPreservesOriginalPairAcrossConcurrentLostAcknowledgement(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_refresh_recovery:refresh-recovery@example.com:Refresh Recovery")
	original := authorizeCLI(t, router, cookies)
	attempt := strings.Repeat("a", 64)
	// Discard the original response just as an interrupted caller would; the
	// real service has already committed the rotation in PostgreSQL.
	issued := refreshAttemptTokens(t, refreshAttemptRequest(router, original.RefreshToken, attempt))
	var wait sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wait.Add(1)
		go func() { defer wait.Done(); responses <- refreshAttemptRequest(router, original.RefreshToken, attempt) }()
	}
	wait.Wait()
	close(responses)
	for response := range responses {
		recovered := refreshAttemptTokens(t, response)
		if recovered.AccessToken != issued.AccessToken || recovered.RefreshToken != issued.RefreshToken || recovered.CLIClientSessionID != original.CLIClientSessionID || recovered.ExpiresIn > issued.ExpiresIn {
			t.Fatal("recovery changed pair, session or expiry")
		}
	}
	var rows, plaintext int
	if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.cli_refresh_tokens WHERE cli_client_session_id=$1`, original.CLIClientSessionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatal("recovery rotated token again")
	}
	if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.client_refresh_redemptions WHERE position($1::bytea in response_ciphertext)>0 OR position($2::bytea in response_ciphertext)>0`, []byte(issued.AccessToken), []byte(issued.RefreshToken)).Scan(&plaintext); err != nil {
		t.Fatal(err)
	}
	if plaintext != 0 {
		t.Fatal("raw credential persisted in recovery table")
	}
	// Offline longer than the access lifetime still recovers the valid exact
	// successor refresh, then obtains new access through a normal new rotation.
	if _, err := store.SQL().Exec(`UPDATE paperboat.client_refresh_redemptions SET access_expires_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	recovered := refreshAttemptTokens(t, refreshAttemptRequest(router, original.RefreshToken, attempt))
	if recovered.ExpiresIn != 0 || recovered.RefreshToken != issued.RefreshToken {
		t.Fatal("offline recovery extended access or changed refresh")
	}
	next := refreshAttemptTokens(t, refreshAttemptRequest(router, recovered.RefreshToken, strings.Repeat("b", 64)))
	if next.CLIClientSessionID != original.CLIClientSessionID || next.ExpiresIn <= 0 || next.RefreshToken == recovered.RefreshToken {
		t.Fatal("offline recovery could not normally rotate successor")
	}
}

func TestRefreshRecoveryDeniesInvalidatedSuccessorAndTampering(t *testing.T) {
	for _, action := range []string{"revoked", "suspended", "rotated", "expired", "tampered"} {
		t.Run(action, func(t *testing.T) {
			store, router := newAuthIntegrationRouter(t)
			cookies := loginCookies(t, router, "workos_refresh_denied:refresh-denied@example.com:Refresh Denied")
			original := authorizeCLI(t, router, cookies)
			attempt := strings.Repeat("a", 64)
			issued := refreshAttemptTokens(t, refreshAttemptRequest(router, original.RefreshToken, attempt))
			want := http.StatusUnauthorized
			var err error
			switch action {
			case "revoked":
				_, err = store.SQL().Exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now(),revocation_reason='test' WHERE id=$1`, original.CLIClientSessionID)
			case "suspended":
				_, err = store.SQL().Exec(`UPDATE paperboat.users SET status='suspended' WHERE id=(SELECT user_id FROM paperboat.cli_client_sessions WHERE id=$1)`, original.CLIClientSessionID)
			case "rotated":
				refreshAttemptTokens(t, refreshAttemptRequest(router, issued.RefreshToken, strings.Repeat("b", 64)))
			case "expired":
				_, err = store.SQL().Exec(`UPDATE paperboat.client_refresh_redemptions SET expires_at=now()-interval '1 second'`)
			case "tampered":
				_, err = store.SQL().Exec(`UPDATE paperboat.client_refresh_redemptions SET response_ciphertext='broken'::bytea`)
				want = http.StatusInternalServerError
			}
			if err != nil {
				t.Fatal(err)
			}
			if response := refreshAttemptRequest(router, original.RefreshToken, attempt); response.Code != want {
				t.Fatalf("recovery status=%d want=%d", response.Code, want)
			}
			if action == "expired" {
				var rows int
				if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.client_refresh_redemptions`).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != 0 {
					t.Fatal("expired recovery not cleaned")
				}
			}
		})
	}
}

// A client that lost the response and its attempt journal retries the rotated token with a fresh attempt id: inside
// the reuse interval it gets the same successor; an older successor-rotated token or a later replay revokes.
func TestRefreshReuseIntervalRedeliversOnlyRecentRotation(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_refresh_reuse:refresh-reuse@example.com:Refresh Reuse")
	original := authorizeCLI(t, router, cookies)
	issued := refreshAttemptTokens(t, refreshAttemptRequest(router, original.RefreshToken, strings.Repeat("a", 64)))
	again := refreshAttemptTokens(t, refreshAttemptRequest(router, original.RefreshToken, strings.Repeat("c", 64)))
	if again.AccessToken != issued.AccessToken || again.RefreshToken != issued.RefreshToken || again.CLIClientSessionID != original.CLIClientSessionID {
		t.Fatal("reuse inside the interval did not re-deliver the stored successor")
	}
	if _, err := store.SQL().Exec(`UPDATE paperboat.cli_refresh_tokens SET rotated_at=now()-interval '31 seconds' WHERE state='rotated'`); err != nil {
		t.Fatal(err)
	}
	if response := refreshAttemptRequest(router, original.RefreshToken, strings.Repeat("d", 64)); response.Code != http.StatusUnauthorized {
		t.Fatalf("replay after the interval status=%d", response.Code)
	}
	var state string
	if err := store.SQL().QueryRow(`SELECT state FROM paperboat.cli_client_sessions WHERE id=$1`, original.CLIClientSessionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "revoked" {
		t.Fatalf("replay after the interval left session %s", state)
	}
}

func TestRefreshReuseIntervalRevokesSupersededSuccessor(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_refresh_reuse2:refresh-reuse2@example.com:Refresh Reuse")
	original := authorizeCLI(t, router, cookies)
	issued := refreshAttemptTokens(t, refreshAttemptRequest(router, original.RefreshToken, strings.Repeat("a", 64)))
	refreshAttemptTokens(t, refreshAttemptRequest(router, issued.RefreshToken, strings.Repeat("b", 64)))
	if response := refreshAttemptRequest(router, original.RefreshToken, strings.Repeat("c", 64)); response.Code != http.StatusUnauthorized {
		t.Fatalf("reuse of an older token status=%d", response.Code)
	}
	var state string
	if err := store.SQL().QueryRow(`SELECT state FROM paperboat.cli_client_sessions WHERE id=$1`, original.CLIClientSessionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "revoked" {
		t.Fatalf("reuse of an older token left session %s", state)
	}
}
