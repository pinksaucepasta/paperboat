package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// LogoutRevocationIncompleteError is returned only after local signout and
// removal of the owned profile and revocation records completed. It carries bounded original read
// failures; it never implies that an erased credential can be retried later.
type LogoutRevocationIncompleteError struct {
	causes     []error
	unreadable int
}

func (*LogoutRevocationIncompleteError) Error() string {
	return "Local signout completed; server revocation could not be confirmed for every session."
}
func (e *LogoutRevocationIncompleteError) Unwrap() []error            { return e.causes }
func (e *LogoutRevocationIncompleteError) UnreadableCredentials() int { return e.unreadable }

// TakeLogoutCredentials atomically removes the active local profile and all
// revocation records for one issuer while returning every readable refresh
// token for best-effort server revocation. Concurrent login and switch
// mutations serialize on the same profile lock, so logout cannot delete a
// session that committed after its snapshot.
func (s ProfileStore) TakeLogoutCredentials(issuer string) (credentials []Credential, resultErr error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return nil, err
	}
	profilePath := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(profilePath)); err != nil {
		return nil, err
	}
	profileLock := newSharedLock(profilePath + ".lock")
	if err := profileLock.Lock(); err != nil {
		return nil, err
	}
	defer func() {
		if err := profileLock.Unlock(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	partial := &LogoutRevocationIncompleteError{}
	recordReadFailure := func(err error) {
		partial.unreadable++
		if len(partial.causes) < 8 {
			partial.causes = append(partial.causes, err)
		}
	}

	activeSessionReturned := ""
	active, activeErr := s.loadNormalized(issuer)
	if activeErr == nil {
		if refresh, refreshErr := s.Secrets.Get(active.RefreshSecretRef); refreshErr == nil && strings.TrimSpace(refresh) != "" {
			accountID := active.Account.ID
			if accountID != "" && !validCredentialID(accountID) {
				accountID = ""
			}
			if err := s.queueRevocationLocked(issuer, active.CLIClientSessionID, refresh, accountID); err != nil {
				return nil, err
			}
			credentials = append(credentials, Credential{RefreshToken: refresh})
			activeSessionReturned = active.CLIClientSessionID
		} else {
			if refreshErr == nil {
				refreshErr = ErrSecretNotFound
			}
			recordReadFailure(refreshErr)
		}
		var cleanupErrs []error
		cleanupErrs = append(cleanupErrs,
			s.Secrets.Delete(active.AccessSecretRef),
			s.Secrets.Delete(active.RefreshSecretRef),
			s.DeleteManagedSSHIdentity(active.Issuer, active.CLIClientSessionID),
			s.DeletePeerEndpointIdentity(active.Issuer, active.CLIClientSessionID),
			s.DeletePeerAccountRoot(active.Issuer, active.Account.ID),
		)
		for _, ref := range active.ObsoleteSecretRefs {
			cleanupErrs = append(cleanupErrs, s.Secrets.Delete(ref))
		}
		if removeErr := os.Remove(profilePath); removeErr != nil && !os.IsNotExist(removeErr) {
			cleanupErrs = append(cleanupErrs, removeErr)
		}
		if err := errors.Join(cleanupErrs...); err != nil {
			return nil, errors.Join(err, errors.Join(partial.causes...))
		}
	} else if !errors.Is(activeErr, ErrNoCredentials) {
		return nil, activeErr
	}

	records, recordsErr := s.pendingRevocationsLocked(issuer, "")
	if recordsErr == nil {
		for _, record := range records {
			if record.CLIClientSessionID == activeSessionReturned {
				continue
			}
			credential, credentialErr := s.PendingRevocationCredential(record)
			if credentialErr == nil && strings.TrimSpace(credential.RefreshToken) != "" {
				credentials = append(credentials, credential)
			} else {
				if credentialErr == nil {
					credentialErr = ErrSecretNotFound
				}
				recordReadFailure(credentialErr)
			}
		}
	} else {
		recordReadFailure(recordsErr)
	}
	// Local logout must not be trapped by corrupt historical metadata. The
	// profile lock prevents any concurrent auth mutation from adding a record
	// between the snapshot above and this cleanup.
	if err := s.DiscardPendingRevocations(issuer); err != nil {
		return nil, errors.Join(err, errors.Join(partial.causes...))
	}
	if partial.unreadable != 0 {
		return credentials, partial
	}
	return credentials, nil
}
