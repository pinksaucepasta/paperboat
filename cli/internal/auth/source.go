package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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
	retry    *refreshRetry
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
	if copy.retry == nil {
		copy.retry = &refreshRetry{}
	}
	return &copy
}

func (s *Source) Credential() (config.Credential, error) {
	return s.credential(refreshBefore)
}

func (s *Source) Refresh() (config.Credential, error) {
	return s.credential(100 * 365 * 24 * time.Hour)
}

func (s *Source) credential(refreshWindow time.Duration) (result config.Credential, resultErr error) {
	var credential config.Credential
	var err error
	if s.lifetime != nil && s.lifetime.Err() != nil {
		return credential, accountCredentialFailure(s.lifetime.Err())
	}
	if s.retry != nil {
		s.retry.mu.Lock()
		defer s.retry.mu.Unlock()
		current, readErr := s.Store.CredentialFor(s.Issuer)
		if readErr != nil {
			return credential, accountCredentialFailure(readErr)
		}
		fingerprint := sha256.Sum256([]byte(current.AccessToken + "\x00" + current.RefreshToken + "\x00" + current.ExpiresAt.UTC().Format(time.RFC3339Nano)))
		if fingerprint != s.retry.fingerprint {
			s.retry.fingerprint = fingerprint
			s.retry.failure, s.retry.delay, s.retry.signedOut = nil, 0, false
		}
		if s.retry.failure != nil && (s.retry.signedOut || time.Now().Before(s.retry.next)) {
			return credential, s.retry.failure
		}
		defer func() {
			if err == nil {
				s.retry.failure, s.retry.delay, s.retry.signedOut = nil, 0, false
				return
			}
			failure := accountCredentialFailure(err)
			s.retry.signedOut = permanentRefreshRejection(err)
			if s.retry.signedOut {
				failure = &SignedOutError{Cause: failure}
			}
			s.retry.failure = failure
			resultErr = failure
			s.retry.delay = min(max(s.retry.delay*2, 15*time.Second), 5*time.Minute)
			s.retry.next = time.Now().Add(s.retry.delay)
		}()
	}
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
		err = config.ErrNoCredentials
		return config.Credential{}, accountCredentialFailure(err)
	}
	return credential, accountCredentialFailure(err)
}

// SignedOutError requires a changed saved credential before the runtime retries.
// Only the stable message is public; the original typed cause remains inspectable.
type SignedOutError struct{ Cause error }

func (*SignedOutError) Error() string   { return "Paperboat is signed out; sign in again" }
func (e *SignedOutError) Unwrap() error { return e.Cause }

type refreshRetry struct {
	mu          sync.Mutex
	fingerprint [32]byte
	failure     error
	signedOut   bool
	delay       time.Duration
	next        time.Time
}

func permanentRefreshRejection(err error) bool {
	var rejection *api.APIError
	if !errors.As(err, &rejection) {
		return false
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboatd", "machine_control", "control_request", "control_request_failed", err)
	if fault.Outcome != "rejected" {
		return false
	}
	return rejection.Status == http.StatusUnauthorized || rejection.Status == http.StatusBadRequest && rejection.Code == "invalid_grant"
}
