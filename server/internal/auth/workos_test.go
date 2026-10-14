package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestHTTPWorkOSVerifierExchangesAuthorizationCode(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"sid": "session_123", "sub": "user_123", "iss": "https://api.workos.com/", "exp": time.Now().Add(time.Hour).Unix()})
	accessToken := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	var requestBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/user_management/authenticate" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"user_123","email":"user@example.com","email_verified":true,"first_name":"Test","last_name":"User"},"authentication_method":"Passkey","impersonator":{"email":"operator@example.com"},"refresh_token":"refresh_123","access_token":"` + accessToken + `"}`))
	}))
	t.Cleanup(server.Close)

	profile, err := HTTPWorkOSVerifier{
		BaseURL:  server.URL,
		ClientID: "client_123",
		APIKey:   "secret_123",
	}.VerifyCallback(context.Background(), CallbackInput{Code: "code_123", RedirectURI: "https://dashboard.example/callback", CodeVerifier: "verifier_123"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Subject != "user_123" || profile.Email != "user@example.com" || profile.DisplayName != "Test User" {
		t.Fatalf("profile = %+v", profile)
	}
	if !profile.EmailVerified {
		t.Fatal("verified email claim was lost")
	}
	if profile.AuthenticationMethod != "Passkey" || !profile.Impersonator {
		t.Fatalf("assurance fields = %+v", profile)
	}
	if profile.SessionID != "session_123" {
		t.Fatalf("session id = %q", profile.SessionID)
	}
	if profile.RefreshToken != "refresh_123" || profile.AccessExpiresAt.Before(time.Now()) {
		t.Fatal("provider session credentials missing")
	}
	if requestBody["grant_type"] != "authorization_code" || requestBody["code"] != "code_123" || requestBody["client_id"] != "client_123" || requestBody["client_secret"] != "secret_123" {
		t.Fatalf("request body = %#v", requestBody)
	}
	if _, ok := requestBody["redirect_uri"]; ok {
		t.Fatalf("unexpected redirect_uri in request body: %#v", requestBody)
	}
	if requestBody["code_verifier"] != "verifier_123" {
		t.Fatalf("code verifier missing: %#v", requestBody)
	}
}

func TestHTTPWorkOSVerifierRefreshesSession(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"sid": "session_123", "sub": "user_123", "iss": "https://api.workos.com/", "exp": time.Now().Add(time.Hour).Unix()})
	accessToken := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_secret"] != "secret_123" {
			t.Fatalf("invalid refresh request: %#v", body)
		}
		_, _ = w.Write([]byte(`{"user":{"id":"user_123","email":"user@example.com","email_verified":true},"access_token":"` + accessToken + `","refresh_token":"new-refresh"}`))
	}))
	defer server.Close()
	profile, err := (HTTPWorkOSVerifier{BaseURL: server.URL, ClientID: "client_123", APIKey: "secret_123"}).RefreshSession(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Subject != "user_123" || profile.SessionID != "session_123" || profile.RefreshToken != "new-refresh" || !profile.EmailVerified {
		t.Fatalf("bad refresh profile: %+v", profile)
	}
}

func TestWorkOSTokenClaimsAcceptsHTTPSCustomIssuer(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"sid": "session_123", "sub": "user_123", "iss": "https://signin.example.test/", "exp": time.Now().Add(time.Hour).Unix()})
	accessToken := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	if sessionID, _, reason := workOSTokenClaims(accessToken, "user_123"); sessionID != "session_123" || reason != "" {
		t.Fatalf("custom issuer rejected: %s", reason)
	}
	if _, _, reason := workOSTokenClaims(accessToken, "user_other"); reason != "subject_mismatch" {
		t.Fatalf("subject mismatch reason = %q", reason)
	}
}

func TestHTTPWorkOSVerifierRejectsRevokedRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	_, err := (HTTPWorkOSVerifier{BaseURL: server.URL, ClientID: "client_123", APIKey: "secret_123"}).RefreshSession(context.Background(), "old-refresh")
	if !errors.Is(err, ErrWorkOSInvalidSession) {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkOSLogoutURL(t *testing.T) {
	verifier := HTTPWorkOSVerifier{BaseURL: "https://api.workos.com"}
	if got := verifier.LogoutURL("session_123"); got != "https://api.workos.com/user_management/sessions/logout?session_id=session_123" {
		t.Fatalf("logout URL = %q", got)
	}
	for _, sessionID := range []string{"", "../../other", "session_123?return_to=https://attacker.invalid"} {
		if got := verifier.LogoutURL(sessionID); got != "" {
			t.Fatalf("accepted invalid session ID: %q", got)
		}
	}
	if got := (HTTPWorkOSVerifier{BaseURL: "http://api.workos.com"}).LogoutURL("session_123"); got != "" {
		t.Fatal("accepted insecure provider URL")
	}
}

func TestHTTPWorkOSVerifierRejectsRedirectsAndOversizedResponses(t *testing.T) {
	var forwarded atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Store(true); w.WriteHeader(200) }))
	defer sink.Close()
	for _, mode := range []string{"redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
					return
				}
				_, _ = w.Write([]byte(`{"unused":"` + strings.Repeat("x", 1<<20) + `"}`))
			}))
			defer upstream.Close()
			_, err := (HTTPWorkOSVerifier{BaseURL: upstream.URL, ClientID: "client_test", APIKey: "test-secret", HTTPClient: upstream.Client()}).VerifyCallback(context.Background(), CallbackInput{Code: "test-code", CodeVerifier: "test-verifier"})
			if err == nil {
				t.Fatal("unsafe provider response accepted")
			}
			if forwarded.Load() {
				t.Fatal("authentication secrets followed redirect")
			}
		})
	}
}

func TestHTTPWorkOSVerifierPreservesBoundedProviderStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("SECRET_PROVIDER_BODY"))
			}))
			defer server.Close()
			_, err := (HTTPWorkOSVerifier{BaseURL: server.URL, ClientID: "client", APIKey: "secret"}).VerifyCallback(context.Background(), CallbackInput{Code: "code"})
			var verification *WorkOSVerificationError
			if !errors.As(err, &verification) || verification.HTTPStatus != status {
				t.Fatalf("verification error = %#v, want status %d", err, status)
			}
			if verification.Reason != "provider_http_error" || verification.Error() != "workos verification failed" || strings.Contains(verification.Error(), "SECRET_PROVIDER_BODY") {
				t.Fatalf("provider error contains unbounded response data: %+v", verification)
			}
			wantDiagnostic := http.StatusBadGateway
			if status == http.StatusBadRequest {
				wantDiagnostic = status
			}
			if got := verification.DiagnosticStatus(); got != wantDiagnostic {
				t.Fatalf("diagnostic status = %d, want %d", got, wantDiagnostic)
			}
		})
	}
}

type workosErrorRoundTripper struct{ err error }

func (t workosErrorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func TestHTTPWorkOSVerifierWrapsTransportCauseWithoutExposingText(t *testing.T) {
	transportFailure := errors.New("SECRET_TRANSPORT_DETAIL")
	_, err := (HTTPWorkOSVerifier{
		BaseURL: "https://api.workos.com", ClientID: "client", APIKey: "secret",
		HTTPClient: &http.Client{Transport: workosErrorRoundTripper{err: transportFailure}},
	}).VerifyCallback(context.Background(), CallbackInput{Code: "code"})
	var verification *WorkOSVerificationError
	if !errors.As(err, &verification) || !errors.Is(err, transportFailure) {
		t.Fatalf("transport cause was not preserved: %v", err)
	}
	if verification.Reason != "provider_transport" || verification.Error() != "workos verification failed" || strings.Contains(verification.Error(), "SECRET_TRANSPORT_DETAIL") {
		t.Fatalf("transport error exposed details: %+v", verification)
	}
	if verification.DiagnosticStatus() != http.StatusBadGateway {
		t.Fatalf("transport diagnostic status = %d", verification.DiagnosticStatus())
	}
}

type workosReadFailureBody struct{ err error }

func (b workosReadFailureBody) Read([]byte) (int, error) { return 0, b.err }
func (workosReadFailureBody) Close() error               { return nil }

type workosReadFailureRoundTripper struct{ err error }

func (t workosReadFailureRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: workosReadFailureBody{err: t.err}}, nil
}

func TestHTTPWorkOSVerifierPreservesResponseReadCause(t *testing.T) {
	_, err := (HTTPWorkOSVerifier{
		BaseURL: "https://api.workos.com", ClientID: "client", APIKey: "secret",
		HTTPClient: &http.Client{Transport: workosReadFailureRoundTripper{err: syscall.ECONNRESET}},
	}).VerifyCallback(context.Background(), CallbackInput{Code: "code"})
	var verification *WorkOSVerificationError
	if !errors.As(err, &verification) || !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("response read cause was not preserved: %v", err)
	}
	if verification.Reason != "response_read" || verification.Error() != "workos verification failed" || verification.DiagnosticStatus() != http.StatusBadGateway {
		t.Fatalf("response read failure is not safely classified: %+v", verification)
	}
}
