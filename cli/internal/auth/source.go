package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

const refreshBefore = 60 * time.Second

// CredentialFailure identifies account-token custody, distinct from private
// pairing custody. Its cause remains available for complete classification.
type CredentialFailure struct{ Cause error }

func (*CredentialFailure) Error() string { return "Paperboat account credentials are unavailable" }
func (e *CredentialFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func accountCredentialFailure(cause error) error {
	if cause == nil {
		return nil
	}
	if _, owned := cause.(*CredentialFailure); owned {
		return cause
	}
	return &CredentialFailure{Cause: cause}
}

type Source struct {
	lifetime context.Context
	Store    config.ProfileStore
	Issuer   string
}

func NewSource(cfg *config.Config) (*Source, error) {
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return nil, err
	}
	return &Source{Store: store, Issuer: cfg.ServerURL}, nil
}

// WithContext binds refresh work to the owning runtime's lifetime. The returned
// source shares durable credential storage, but does not outlive daemon shutdown.
func (s *Source) WithContext(ctx context.Context) *Source {
	copy := *s
	copy.lifetime = ctx
	return &copy
}

func (s *Source) Credential() (config.Credential, error) {
	return s.credential(refreshBefore)
}

func (s *Source) Refresh() (config.Credential, error) {
	return s.credential(100 * 365 * 24 * time.Hour)
}

func (s *Source) credential(refreshWindow time.Duration) (config.Credential, error) {
	var credential config.Credential
	var err error
	for range 2 {
		credential, err = s.Store.CredentialWithRefresh(s.Issuer, refreshWindow, func(current config.Credential, attemptID string) (config.Credential, string, error) {
			parent := s.lifetime
			if parent == nil {
				parent = context.Background()
			}
			ctx, cancel := context.WithTimeout(parent, 30*time.Second)
			defer cancel()
			tokens, err := api.RefreshToken(ctx, s.Issuer, current.RefreshToken, attemptID, nil)
			if err != nil {
				return config.Credential{}, "", fmt.Errorf("refresh Paperboat session: %w", err)
			}
			expires := time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)
			return config.Credential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, TokenType: tokens.TokenType, ExpiresAt: expires}, tokens.CLIClientSessionID, nil
		})
		if err != nil || time.Now().Before(credential.ExpiresAt) {
			break
		}
		// An offline interrupted rotation can recover an expired access token with
		// its still-valid successor refresh. Commit it first, then rotate normally.
	}
	if err == nil && !time.Now().Before(credential.ExpiresAt) {
		return config.Credential{}, accountCredentialFailure(config.ErrNoCredentials)
	}
	return credential, accountCredentialFailure(err)
}
