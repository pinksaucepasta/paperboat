package github

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
)

var ErrOAuthRefreshUnavailable = errors.New("GitHub token renewal is unavailable; retry later")

// A provider refresh rotates a single-use credential. Never execute it inside
// an automatically retried transaction. The row lock serializes refresh with
// other refreshes, reconnects and revocations across server instances.
func (s *Service) refreshGitHubToken(ctx context.Context, userID string) (string, string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := s.db.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	defer tx.Rollback()
	var accessCipher, refreshCipher []byte
	var login string
	var scopes []string
	var expiry, refreshExpiry sql.NullTime
	// Scan PostgreSQL arrays through pgx's connection-backed driver via JSON,
	// rather than relying on database/sql's array conversion.
	var scopesJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT token_ciphertext, refresh_token_ciphertext, provider_account_login, array_to_json(scopes), expires_at, refresh_expires_at FROM paperboat.github_oauth_tokens WHERE user_id=$1 AND revoked_at IS NULL FOR UPDATE`, userID).Scan(&accessCipher, &refreshCipher, &login, &scopesJSON, &expiry, &refreshExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, ErrNotConnected
	}
	if err != nil || json.Unmarshal(scopesJSON, &scopes) != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	if len(missingScopes(scopes, s.requiredOAuthScopes())) > 0 {
		return "", "", time.Time{}, ErrMissingScopes
	}
	if !expiry.Valid || expiry.Time.After(s.now().UTC().Add(time.Minute)) {
		token, err := secrets.Decrypt(s.cfg.Secrets.EncryptionKey, accessCipher)
		if err != nil {
			return "", "", time.Time{}, ErrNotConnected
		}
		return token, login, expiry.Time, nil
	}
	if refreshExpiry.Valid && !refreshExpiry.Time.After(s.now().UTC()) {
		return "", "", time.Time{}, ErrNotConnected
	}
	refresh, err := secrets.Decrypt(s.cfg.Secrets.EncryptionKey, refreshCipher)
	if err != nil || refresh == "" {
		return "", "", time.Time{}, ErrNotConnected
	}
	token, err := s.client.RefreshOAuthToken(ctx, OAuthRefreshInput{RefreshToken: refresh, ClientID: s.cfg.Secrets.GitHubClientID, ClientSecret: s.cfg.Secrets.GitHubClientSecret})
	if errors.Is(err, ErrNotConnected) {
		// A definitively invalid refresh token must not be sent again on every
		// background poll. Keep the expired access record for truthful status.
		if _, updateErr := tx.ExecContext(ctx, `UPDATE paperboat.github_oauth_tokens SET refresh_token_ciphertext=NULL, updated_at=now(), version=version+1 WHERE user_id=$1 AND revoked_at IS NULL`, userID); updateErr != nil {
			return "", "", time.Time{}, ErrOAuthRefreshUnavailable
		}
		if tx.Commit() != nil {
			return "", "", time.Time{}, ErrOAuthRefreshUnavailable
		}
		return "", "", time.Time{}, ErrNotConnected
	}
	if err != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	if token.AccessToken == "" || token.RefreshToken == "" || !token.ExpiresAt.After(s.now().UTC().Add(time.Minute)) || len(missingScopes(token.Scopes, s.requiredOAuthScopes())) > 0 {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	accessCipher, err = secrets.Encrypt(s.cfg.Secrets.EncryptionKey, token.AccessToken)
	if err != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	refreshCipher, err = secrets.Encrypt(s.cfg.Secrets.EncryptionKey, token.RefreshToken)
	if err != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	if token.Scopes == nil {
		token.Scopes = []string{}
	}
	var nextRefreshExpiry sql.NullTime
	if !token.RefreshExpiresAt.IsZero() {
		nextRefreshExpiry = sql.NullTime{Time: token.RefreshExpiresAt, Valid: true}
	}
	scopeBytes, _ := json.Marshal(token.Scopes)
	// The locked row cannot be replaced or revoked until this transaction ends.
	_, err = tx.ExecContext(ctx, `UPDATE paperboat.github_oauth_tokens SET token_ciphertext=$2,refresh_token_ciphertext=$3,expires_at=$4,refresh_expires_at=$6,scopes=ARRAY(SELECT jsonb_array_elements_text($5::jsonb)),updated_at=now(),version=version+1 WHERE user_id=$1 AND revoked_at IS NULL`, userID, accessCipher, refreshCipher, token.ExpiresAt, string(scopeBytes), nextRefreshExpiry)
	if err != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	if err = tx.Commit(); err != nil {
		return "", "", time.Time{}, ErrOAuthRefreshUnavailable
	}
	return token.AccessToken, login, token.ExpiresAt, nil
}

func (c HTTPClient) RefreshOAuthToken(ctx context.Context, input OAuthRefreshInput) (OAuthToken, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {input.RefreshToken}, "client_id": {input.ClientID}, "client_secret": {input.ClientSecret}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthToken{}, ErrOAuthRefreshUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := *c.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrOAuthRefreshUnavailable }
	response, err := client.Do(request)
	if err != nil {
		return OAuthToken{}, ErrOAuthRefreshUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		return OAuthToken{}, ErrOAuthRefreshUnavailable
	}
	var data struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Scope            string `json:"scope"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		Error            string `json:"error"`
	}
	if json.Unmarshal(body, &data) != nil {
		return OAuthToken{}, ErrOAuthRefreshUnavailable
	}
	if data.Error == "bad_refresh_token" || data.Error == "invalid_grant" {
		return OAuthToken{}, ErrNotConnected
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || data.Error != "" || data.AccessToken == "" || data.RefreshToken == "" || data.ExpiresIn <= 60 || data.ExpiresIn > 86400 {
		return OAuthToken{}, ErrOAuthRefreshUnavailable
	}
	token := OAuthToken{AccessToken: data.AccessToken, RefreshToken: data.RefreshToken, Scopes: splitScopes(data.Scope), ExpiresAt: time.Now().UTC().Add(time.Duration(data.ExpiresIn) * time.Second)}
	if data.RefreshExpiresIn > 0 && data.RefreshExpiresIn <= 31536000 {
		token.RefreshExpiresAt = time.Now().UTC().Add(time.Duration(data.RefreshExpiresIn) * time.Second)
	}
	return token, nil
}
