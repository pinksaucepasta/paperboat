package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
)

type HTTPWorkOSVerifier struct {
	BaseURL    string
	ClientID   string
	APIKey     string
	HTTPClient *http.Client
}

var workOSSessionIDPattern = regexp.MustCompile(`^session_[A-Za-z0-9]{1,128}$`)

// WorkOSVerificationError contains only fixed categories, a bounded HTTP
// status, and the original transport cause. Its public text never includes
// provider response data or wrapped error messages.
type WorkOSVerificationError struct {
	Reason     string
	HTTPStatus int
	Cause      error
}

func (e *WorkOSVerificationError) Error() string { return "workos verification failed" }
func (e *WorkOSVerificationError) Unwrap() error { return e.Cause }
func (e *WorkOSVerificationError) DiagnosticStatus() int {
	if e.HTTPStatus == http.StatusBadRequest {
		return http.StatusBadRequest
	}
	if e.Reason != "" || e.Cause != nil || e.HTTPStatus >= 400 {
		return http.StatusBadGateway
	}
	return 0
}

var ErrWorkOSImpersonation = errors.New("workos impersonation cannot create a paperboat session")

func workOSTokenClaims(accessToken, subject string) (string, time.Time, string) {
	// These claims are consumed only from the direct, redirect-disabled WorkOS
	// token response over TLS. The access token is never accepted from a browser.
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 || len(parts[1]) > 4096 {
		return "", time.Time{}, "token_shape"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 4096 {
		return "", time.Time{}, "token_payload"
	}
	var claims struct {
		SID string `json:"sid"`
		Sub string `json:"sub"`
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", time.Time{}, "token_claims"
	}
	if !workOSSessionIDPattern.MatchString(claims.SID) {
		return "", time.Time{}, "session_id"
	}
	if claims.Sub != subject {
		return "", time.Time{}, "subject_mismatch"
	}
	issuer, err := url.Parse(claims.Iss)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil {
		return "", time.Time{}, "issuer"
	}
	if claims.Exp <= time.Now().Unix() {
		return "", time.Time{}, "expired_access_token"
	}
	return claims.SID, time.Unix(claims.Exp, 0).UTC(), ""
}

func (v HTTPWorkOSVerifier) LogoutURL(sessionID string) string {
	if !workOSSessionIDPattern.MatchString(sessionID) {
		return ""
	}
	base, err := url.Parse(v.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return ""
	}
	base.Path = "/user_management/sessions/logout"
	base.RawPath = ""
	base.RawQuery = url.Values{"session_id": {sessionID}}.Encode()
	return base.String()
}

func (v HTTPWorkOSVerifier) VerifyCallback(ctx context.Context, input CallbackInput) (WorkOSProfile, error) {
	if strings.TrimSpace(v.BaseURL) == "" || strings.TrimSpace(v.ClientID) == "" || strings.TrimSpace(v.APIKey) == "" {
		return WorkOSProfile{}, errors.New("workos base URL, client ID, and API key must be configured")
	}
	return v.authenticate(ctx, map[string]string{"client_id": v.ClientID, "client_secret": v.APIKey, "code": input.Code, "grant_type": "authorization_code", "code_verifier": input.CodeVerifier})
}

var ErrWorkOSInvalidSession = errors.New("workos session is no longer valid")

func (v HTTPWorkOSVerifier) RefreshSession(ctx context.Context, refreshToken string) (WorkOSProfile, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return WorkOSProfile{}, ErrWorkOSInvalidSession
	}
	return v.authenticate(ctx, map[string]string{"client_id": v.ClientID, "client_secret": v.APIKey, "refresh_token": refreshToken, "grant_type": "refresh_token"})
}

func (v HTTPWorkOSVerifier) authenticate(ctx context.Context, payload map[string]string) (WorkOSProfile, error) {
	client := v.HTTPClient
	if client == nil {
		client = serverdiagnostics.DefaultProviderClient("workos")
	}
	// Do not forward the authorization code, PKCE verifier or client secret to a redirect.
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &boundedClient
	if strings.TrimSpace(payload["code_verifier"]) == "" {
		delete(payload, "code_verifier")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return WorkOSProfile{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(v.BaseURL, "/")+"/user_management/authenticate", bytes.NewReader(body))
	if err != nil {
		return WorkOSProfile{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: "provider_transport", Cause: err}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		if payload["grant_type"] == "refresh_token" && res.StatusCode == http.StatusBadRequest {
			var providerError struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&providerError)
			if providerError.Error == "invalid_grant" || providerError.Error == "invalid_refresh_token" {
				return WorkOSProfile{}, ErrWorkOSInvalidSession
			}
		}
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: "provider_http_error", HTTPStatus: res.StatusCode}
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		User         struct {
			ID            string `json:"id"`
			Subject       string `json:"subject"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
			FirstName     string `json:"first_name"`
			LastName      string `json:"last_name"`
			Name          string `json:"name"`
		} `json:"user"`
		AuthenticationMethod string    `json:"authentication_method"`
		Impersonator         *struct{} `json:"impersonator"`
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: "response_read", Cause: err}
	}
	if len(raw) > 1<<20 {
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: "response_size"}
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: "response_json"}
	}
	subject := response.User.Subject
	if subject == "" {
		subject = response.User.ID
	}
	sessionID, tokenExpiry, claimError := workOSTokenClaims(response.AccessToken, subject)
	if claimError != "" {
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: claimError}
	}
	if response.RefreshToken == "" {
		return WorkOSProfile{}, &WorkOSVerificationError{Reason: "missing_refresh_token"}
	}
	name := strings.TrimSpace(response.User.Name)
	if name == "" {
		name = strings.TrimSpace(response.User.FirstName + " " + response.User.LastName)
	}
	return WorkOSProfile{Subject: subject, Email: response.User.Email, EmailVerified: response.User.EmailVerified, DisplayName: name, AuthenticationMethod: response.AuthenticationMethod, Impersonator: response.Impersonator != nil, SessionID: sessionID, RefreshToken: response.RefreshToken, AccessExpiresAt: tokenExpiry}, nil
}
