package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
)

func approveRecoveryGrant(t *testing.T, router http.Handler, cookies []*http.Cookie, grant deviceGrant) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/device/requests/"+grant.UserCode+"/approve", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookies))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status=%d", rec.Code)
	}
}
func cancelRecoveryGrant(t *testing.T, router http.Handler, code string, want int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"client_id": "paperboat", "device_code": code})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/device/cancel", bytes.NewReader(body)))
	if rec.Code != want {
		t.Fatalf("cancel status=%d want=%d", rec.Code, want)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cancel lacks no-store")
	}
}
func TestDeviceRecoveryReturnsOriginalPairAndRejectsRevokedOrRotated(t *testing.T) {
	for _, action := range []string{"revoke", "rotate", "suspend", "expire", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			store, router := newAuthIntegrationRouter(t)
			cookies := loginCookies(t, router, "workos_recovery:recovery@example.com:Recovery User")
			grant := authorizeDevice(t, router)
			approveRecoveryGrant(t, router, cookies, grant)
			tokens := pollDevice(t, router, grant.DeviceCode, http.StatusOK)
			if _, err := store.SQL().Exec(`UPDATE paperboat.device_token_redemptions SET access_expires_at=now()+interval '2 minutes' WHERE cli_client_session_id=$1`, tokens.CLIClientSessionID); err != nil {
				t.Fatal(err)
			}
			recovered := pollDevice(t, router, grant.DeviceCode, http.StatusOK)
			if recovered.AccessToken != tokens.AccessToken || recovered.RefreshToken != tokens.RefreshToken || recovered.CLIClientSessionID != tokens.CLIClientSessionID {
				t.Fatal("recovery created different credentials")
			}
			if recovered.ExpiresIn > 120 || recovered.ExpiresIn <= 0 {
				t.Fatalf("remaining expiry=%d", recovered.ExpiresIn)
			}
			var raw int
			if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.device_token_redemptions WHERE position($1::bytea in response_ciphertext)>0 OR position($2::bytea in response_ciphertext)>0`, []byte(tokens.AccessToken), []byte(tokens.RefreshToken)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if raw != 0 {
				t.Fatal("recovery row contains plaintext credential")
			}
			switch action {
			case "revoke":
				cancelRecoveryGrant(t, router, grant.DeviceCode, http.StatusNoContent)
			case "rotate":
				refreshDevice(t, router, tokens.RefreshToken, http.StatusOK)
			case "suspend":
				if _, err := store.SQL().Exec(`UPDATE paperboat.users SET status='suspended' WHERE id=$1`, userIDByEmail(t, store, "recovery@example.com")); err != nil {
					t.Fatal(err)
				}
			case "cleanup":
				if _, err := store.SQL().Exec(`UPDATE paperboat.device_token_redemptions SET expires_at=now()-interval '1 second'`); err != nil {
					t.Fatal(err)
				}
			case "expire":
				if _, err := store.SQL().Exec(`UPDATE paperboat.device_grants SET expires_at=now()-interval '1 second'`); err != nil {
					t.Fatal(err)
				}
			}
			pollDevice(t, router, grant.DeviceCode, http.StatusBadRequest)
			if action == "cleanup" {
				var rows int
				if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.device_token_redemptions`).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != 0 {
					t.Fatalf("expired recovery rows=%d", rows)
				}
			}
		})
	}
}
func TestDeviceCancelBeforeAndAfterConsumption(t *testing.T) {
	for _, state := range []string{"pending", "approved", "consumed"} {
		t.Run(state, func(t *testing.T) {
			store, router := newAuthIntegrationRouter(t)
			cookies := loginCookies(t, router, "workos_cancel:cancel@example.com:Cancel User")
			grant := authorizeDevice(t, router)
			if state != "pending" {
				approveRecoveryGrant(t, router, cookies, grant)
			}
			var tokens tokenResponse
			if state == "consumed" {
				tokens = pollDevice(t, router, grant.DeviceCode, http.StatusOK)
			}
			cancelRecoveryGrant(t, router, "wrong-code", http.StatusBadRequest)
			cancelRecoveryGrant(t, router, grant.DeviceCode, http.StatusNoContent)
			cancelRecoveryGrant(t, router, grant.DeviceCode, http.StatusNoContent)
			pollDevice(t, router, grant.DeviceCode, http.StatusBadRequest)
			if tokens.AccessToken != "" {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
				req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("cancelled session authenticated status=%d", rec.Code)
				}
			}
			var active int
			if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.cli_client_sessions WHERE state='active'`).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != 0 {
				t.Fatalf("active sessions after cancellation=%d", active)
			}
		})
	}
}
func TestInteractiveEnrollmentTokenEndpointRemoved(t *testing.T) {
	_, router := newAuthIntegrationRouter(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/enrollment/token", bytes.NewBufferString(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retired enrollment endpoint status=%d", rec.Code)
	}
}

func TestDeviceCancelConcurrentWithTokenConsumption(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_cancel_race:cancel-race@example.com:Cancel Race")
	grant := authorizeDevice(t, router)
	approveRecoveryGrant(t, router, cookies, grant)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, path := range []string{"token", "cancel"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"client_id": "paperboat", "device_code": grant.DeviceCode})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/device/"+path, bytes.NewReader(body)))
			statuses <- rec.Code
		}(path)
	}
	wg.Wait()
	close(statuses)
	cancelled := false
	for status := range statuses {
		if status == http.StatusNoContent {
			cancelled = true
		}
		if status != http.StatusOK && status != http.StatusNoContent && status != http.StatusBadRequest {
			t.Fatalf("concurrent status=%d", status)
		}
	}
	if !cancelled {
		t.Fatal("cancellation did not succeed")
	}
	var active int
	if err := store.SQL().QueryRow(`SELECT count(*) FROM paperboat.cli_client_sessions WHERE state='active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("concurrent cancellation left active sessions=%d", active)
	}
}
