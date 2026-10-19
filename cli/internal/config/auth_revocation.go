package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type PendingRevocation struct {
	Version            int       `json:"version"`
	Issuer             string    `json:"issuer"`
	CLIClientSessionID string    `json:"cli_client_session_id"`
	AccountID          string    `json:"account_id,omitempty"`
	RefreshSecretRef   string    `json:"refresh_secret_ref"`
	CreatedAt          time.Time `json:"created_at"`
	ServerRevoked      bool      `json:"server_revoked,omitempty"`
	Cancelled          bool      `json:"cancelled,omitempty"`
}

func pendingRevocationKey(issuer, cliClientSessionID string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + cliClientSessionID))
	return hex.EncodeToString(sum[:16])
}

func (s ProfileStore) pendingRevocationPath(issuer, cliClientSessionID string) string {
	return filepath.Join(s.Path, "pending-revocations", profileKey(issuer)+"-"+pendingRevocationKey(issuer, cliClientSessionID)+".json")
}

func (s ProfileStore) existingPendingRevocationPath(issuer, cliClientSessionID string) string {
	path := s.pendingRevocationPath(issuer, cliClientSessionID)
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return path
	}
	return filepath.Join(s.Path, "pending-revocations", pendingRevocationKey(issuer, cliClientSessionID)+".json")
}

// QueueRevocation durably retains a refresh token until server revocation is
// confirmed. Re-queueing the same client session is idempotent.
func (s ProfileStore) QueueRevocation(issuer, cliClientSessionID, refreshToken string, accountID ...string) (resultErr error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	profilePath := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(profilePath)); err != nil {
		return err
	}
	profileLock := newSharedLock(profilePath + ".lock")
	if err := profileLock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, profileLock.Unlock()) }()
	return s.queueRevocationLocked(issuer, cliClientSessionID, refreshToken, accountID...)
}

// queueRevocationLocked writes a revocation while the issuer profile lock is
// already held. Keeping one lock order prevents logout, switch, and recovery
// from losing each other's session records.
func (s ProfileStore) queueRevocationLocked(issuer, cliClientSessionID, refreshToken string, accountID ...string) (resultErr error) {
	if strings.TrimSpace(cliClientSessionID) == "" || strings.TrimSpace(refreshToken) == "" {
		return errors.New("pending revocation requires client session id and refresh token")
	}
	account := ""
	if len(accountID) > 0 {
		account = accountID[0]
		if account != "" && !validCredentialID(account) {
			return errors.New("pending revocation account id is invalid")
		}
	}
	path := s.pendingRevocationPath(issuer, cliClientSessionID)
	lock := newSharedLock(path + ".lock")
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	ref := pendingRefreshRef(issuer, cliClientSessionID)
	if err := s.Secrets.Set(ref, refreshToken); err != nil {
		return fmt.Errorf("store pending revocation token: %w", err)
	}
	record := PendingRevocation{Version: ProfileVersion, Issuer: issuer, CLIClientSessionID: cliClientSessionID, AccountID: account, RefreshSecretRef: ref, CreatedAt: time.Now().UTC()}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o600); err != nil {
		_ = s.Secrets.Delete(ref)
		return err
	}
	return nil
}

func (s ProfileStore) PendingRevocations(issuer string) (records []PendingRevocation, resultErr error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return nil, err
	}
	profileLock := newSharedLock(s.profilePath(issuer) + ".lock")
	if err := ensureProfileDirectory(filepath.Dir(s.profilePath(issuer))); err != nil {
		return nil, err
	}
	if err := profileLock.Lock(); err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, profileLock.Unlock()) }()
	activeSessionID := ""
	active, err := s.loadNormalized(issuer)
	if err == nil {
		activeSessionID = active.CLIClientSessionID
	} else if !errors.Is(err, ErrNoCredentials) {
		return nil, err
	}
	return s.pendingRevocationsLocked(issuer, activeSessionID)
}

func (s ProfileStore) pendingRevocationsLocked(issuer, activeSessionID string) ([]PendingRevocation, error) {
	var records []PendingRevocation
	dir := filepath.Join(s.Path, "pending-revocations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := profileKey(issuer) + "-"
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record PendingRevocation
		if err := json.Unmarshal(b, &record); err != nil {
			return nil, fmt.Errorf("parse pending revocation: %w", err)
		}
		if record.Issuer == issuer {
			if record.Version != ProfileVersion {
				return nil, errors.New("unsupported pending revocation version")
			}
			if record.CLIClientSessionID == activeSessionID {
				// Revoking any refresh token from the active session can revoke
				// the whole rotated-token family. Keep every same-session record
				// staged until a different profile becomes authoritative.
				continue
			}
			records = append(records, record)
		}
	}
	return records, nil
}

func (s ProfileStore) PendingRevocationCredential(record PendingRevocation) (Credential, error) {
	refresh, err := s.Secrets.Get(record.RefreshSecretRef)
	if err != nil {
		return Credential{}, fmt.Errorf("read pending revocation token: %w", err)
	}
	return Credential{RefreshToken: refresh, TokenType: "Bearer"}, nil
}

func (s ProfileStore) CompleteRevocation(record PendingRevocation) (resultErr error) {
	issuer, err := NormalizeIssuer(record.Issuer)
	if err != nil {
		return err
	}
	profilePath := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(profilePath)); err != nil {
		return err
	}
	profileLock := newSharedLock(profilePath + ".lock")
	if err := profileLock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, profileLock.Unlock()) }()
	path := s.existingPendingRevocationPath(record.Issuer, record.CLIClientSessionID)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	lock := newSharedLock(path + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	active, activeErr := s.loadNormalized(issuer)
	if err := s.Secrets.Delete(record.RefreshSecretRef); err != nil {
		return err
	}
	if activeErr != nil || active.CLIClientSessionID != record.CLIClientSessionID {
		if err := s.DeleteManagedSSHIdentity(record.Issuer, record.CLIClientSessionID); err != nil {
			return err
		}
		if err := s.DeletePeerEndpointIdentity(record.Issuer, record.CLIClientSessionID); err != nil {
			return err
		}
	}
	if record.AccountID != "" {
		if errors.Is(activeErr, ErrNoCredentials) || activeErr == nil && active.Account.ID != record.AccountID {
			if err := s.DeletePeerAccountRoot(record.Issuer, record.AccountID); err != nil {
				return err
			}
		} else if activeErr != nil {
			return activeErr
		}
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DiscardRevocation removes a queued copy without revoking the server session.
// It is used to roll back an account switch whose active-profile replacement
// did not commit.
func (s ProfileStore) DiscardRevocation(issuer, cliClientSessionID string) (resultErr error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	path := s.existingPendingRevocationPath(issuer, cliClientSessionID)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	lock := newSharedLock(path + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var record PendingRevocation
	if err := json.Unmarshal(b, &record); err != nil {
		return err
	}
	record.Cancelled = true
	b, err = json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := s.Secrets.Delete(record.RefreshSecretRef); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DiscardPendingRevocations removes every local revocation record for issuer.
// Logout uses this after abandoning best-effort server revocation. It tolerates
// partially written metadata so corrupt historical entries cannot trap a user
// in a locally signed-in state.
func (s ProfileStore) DiscardPendingRevocations(issuer string) (resultErr error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.Path, "pending-revocations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ensureProfileDirectory(dir); err != nil {
		return err
	}
	prefix := profileKey(issuer) + "-"
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		lock := newSharedLock(path + ".lock")
		if lockErr := lock.Lock(); lockErr != nil {
			errs = append(errs, lockErr)
			continue
		}
		func() {
			defer func() {
				if unlockErr := lock.Unlock(); unlockErr != nil {
					errs = append(errs, unlockErr)
				}
			}()
			var record PendingRevocation
			if data, readErr := os.ReadFile(path); readErr == nil {
				_ = json.Unmarshal(data, &record)
			}
			if strings.HasPrefix(record.RefreshSecretRef, "revocation-") {
				errs = append(errs, s.Secrets.Delete(record.RefreshSecretRef))
			}
			if record.CLIClientSessionID != "" {
				errs = append(errs, s.DeleteManagedSSHIdentity(issuer, record.CLIClientSessionID))
			}
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				errs = append(errs, removeErr)
			}
		}()
	}
	return errors.Join(errs...)
}

func (s ProfileStore) MarkRevocationSucceeded(record PendingRevocation) (result PendingRevocation, resultErr error) {
	path := s.existingPendingRevocationPath(record.Issuer, record.CLIClientSessionID)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return PendingRevocation{}, err
	}
	lock := newSharedLock(path + ".lock")
	if err := lock.Lock(); err != nil {
		return PendingRevocation{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Unlock())
		if resultErr != nil {
			result = PendingRevocation{}
		}
	}()
	record.ServerRevoked = true
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return PendingRevocation{}, err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o600); err != nil {
		return PendingRevocation{}, err
	}
	return record, nil
}

// QueueActiveRevocation moves the active profile into the durable revocation
// queue before removing its normal credential references.
func (s ProfileStore) QueueActiveRevocation(issuer string) (resultErr error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	profilePath := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(profilePath)); err != nil {
		return err
	}
	lock := newSharedLock(profilePath + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	p, err := s.loadNormalized(issuer)
	if err != nil {
		return err
	}
	pendingPath := s.pendingRevocationPath(p.Issuer, p.CLIClientSessionID)
	if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
		refreshToken, getErr := s.Secrets.Get(p.RefreshSecretRef)
		if getErr != nil {
			return getErr
		}
		if err := s.queueRevocationLocked(p.Issuer, p.CLIClientSessionID, refreshToken, p.Account.ID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var deleteErrs []error
	deleteErrs = append(deleteErrs, s.Secrets.Delete(p.AccessSecretRef), s.Secrets.Delete(p.RefreshSecretRef), s.DeleteManagedSSHIdentity(p.Issuer, p.CLIClientSessionID))
	for _, ref := range p.ObsoleteSecretRefs {
		deleteErrs = append(deleteErrs, s.Secrets.Delete(ref))
	}
	if err := errors.Join(deleteErrs...); err != nil {
		return err
	}
	if err := os.Remove(profilePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
