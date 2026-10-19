package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func (s ProfileStore) CredentialFor(issuer string) (Credential, error) {
	p, err := s.Load(issuer)
	if err != nil {
		return Credential{}, err
	}
	return s.credentialForProfile(p)
}

func (s ProfileStore) credentialForProfile(p Profile) (Credential, error) {
	a, err := s.Secrets.Get(p.AccessSecretRef)
	if err != nil {
		return Credential{}, fmt.Errorf("read access token: %w", err)
	}
	r, err := s.Secrets.Get(p.RefreshSecretRef)
	if err != nil {
		return Credential{}, fmt.Errorf("read refresh token: %w", err)
	}
	credential := Credential{AccessToken: a, RefreshToken: r, TokenType: "Bearer", ExpiresAt: p.AccessExpiresAt}
	if err := validateCredential(credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

type RefreshFunc func(Credential, string) (Credential, string, error)

// CredentialWithRefresh serializes the complete read-refresh-write operation.
// It rechecks expiry after taking the lock because another process may have
// refreshed while this process was waiting.
func (s ProfileStore) CredentialWithRefresh(issuer string, refreshBefore time.Duration, refresh RefreshFunc) (Credential, error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return Credential{}, err
	}
	path := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return Credential{}, err
	}
	lock := newSharedLock(path + ".lock")
	return s.credentialWithRefresh(issuer, refreshBefore, refresh, lock)
}

func (s ProfileStore) credentialWithRefresh(issuer string, refreshBefore time.Duration, refresh RefreshFunc, lock credentialLock) (credential Credential, resultErr error) {
	if issuer == "" || lock == nil {
		return Credential{}, ErrCredentialStoreUnavailable
	}
	path := s.profilePath(issuer)
	if err := lock.Lock(); err != nil {
		return Credential{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Unlock())
		if resultErr != nil {
			credential = Credential{}
		}
	}()
	p, err := s.loadNormalized(issuer)
	if err != nil {
		return Credential{}, err
	}
	cred, err := s.credentialForProfile(p)
	if err != nil {
		return Credential{}, err
	}
	attempt, attemptErr := s.loadRefreshAttempt(issuer)
	if attemptErr != nil && !errors.Is(attemptErr, os.ErrNotExist) {
		return Credential{}, attemptErr
	}
	if attemptErr == nil && (attempt.SessionID != p.CLIClientSessionID || attempt.State == "committed") {
		if err := s.clearRefreshAttempt(issuer, attempt); err != nil {
			return Credential{}, err
		}
		attempt = refreshAttempt{}
	}
	if refresh == nil || attempt.ID == "" && time.Now().Add(refreshBefore).Before(p.AccessExpiresAt) {
		return cred, nil
	}
	attempt, original, err := s.prepareRefreshAttempt(p, cred, attempt)
	if err != nil {
		return Credential{}, err
	}
	next, sessionID, err := refresh(original, attempt.ID)
	if err != nil {
		return Credential{}, err
	}
	if sessionID != p.CLIClientSessionID {
		recoverySessionID := sessionID
		if recoverySessionID == "" {
			recoverySessionID = p.CLIClientSessionID
		}
		if queueErr := s.queueRevocationLocked(issuer, recoverySessionID, next.RefreshToken); queueErr != nil {
			return Credential{}, errors.Join(errors.New("refreshed credential changed client session"), fmt.Errorf("retain rotated credential: %w", queueErr))
		}
		var cleanupErrs []error
		cleanupErrs = append(cleanupErrs, s.clearRefreshAttempt(issuer, attempt))
		cleanupErrs = append(cleanupErrs, s.Secrets.Delete(p.AccessSecretRef), s.Secrets.Delete(p.RefreshSecretRef))
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			cleanupErrs = append(cleanupErrs, removeErr)
		}
		return Credential{}, errors.Join(errors.New("refreshed credential changed client session"), errors.Join(cleanupErrs...))
	}
	p.AccessExpiresAt = next.ExpiresAt
	if err := s.saveLocked(p, next); err != nil {
		return Credential{}, err
	}
	if err := s.clearRefreshAttempt(issuer, attempt); err != nil {
		return Credential{}, err
	}
	return next, nil
}
