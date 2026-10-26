package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/userpaths"
)

const ProfileVersion = 1
const keyringService = "paperboat"

var ErrCredentialStoreUnavailable = errors.New("OS credential store unavailable")
var ErrCredentialRequiresInteractiveLogin = errors.New("credential requires an interactive login to migrate")

var (
	ErrNoCredentials  = errors.New("not signed in to Paperboat")
	ErrProfileExists  = errors.New("Paperboat credential profile already exists")
	ErrProfileChanged = errors.New("Paperboat credential profile changed")
)

type Credential struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    time.Time
}

type Account struct {
	ID          string `json:"id,omitempty"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type Profile struct {
	Version            int       `json:"version"`
	Issuer             string    `json:"issuer"`
	Account            Account   `json:"account,omitempty"`
	CLIClientSessionID string    `json:"cli_client_session_id"`
	AccessExpiresAt    time.Time `json:"access_expires_at"`
	AccessSecretRef    string    `json:"access_secret_ref"`
	RefreshSecretRef   string    `json:"refresh_secret_ref"`
	ObsoleteSecretRefs []string  `json:"obsolete_secret_refs,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type ProfileStore struct {
	Path    string
	Secrets SecretStore
	write   func(string, []byte, os.FileMode) error
}

func NormalizeIssuer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", safeConfigCause("invalid Paperboat server URL", err)
	}
	if u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", errors.New("invalid Paperboat server URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(hostname, ":") {
			u.Host = "[" + hostname + "]"
		} else {
			u.Host = hostname
		}
	} else {
		u.Host = net.JoinHostPort(hostname, port)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func profileKey(issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	return hex.EncodeToString(sum[:16])
}
func newSecretRefs(issuer string) (access, refresh string, err error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", err
	}
	prefix := "profile-" + profileKey(issuer) + "-" + hex.EncodeToString(nonce[:])
	return prefix + "-access", prefix + "-refresh", nil
}

func validateCredential(cred Credential) error {
	if strings.TrimSpace(cred.AccessToken) == "" || strings.TrimSpace(cred.RefreshToken) == "" {
		return errors.New("Paperboat credential requires non-empty access and refresh tokens")
	}
	return nil
}

func (s ProfileStore) writeActiveProfile(path string, data []byte) error {
	if s.write != nil {
		return s.write(path, data, 0o600)
	}
	return atomicWrite(path, data, 0o600)
}

func obsoleteSecretRefs(previous Profile, next Profile) []string {
	seen := map[string]struct{}{next.AccessSecretRef: {}, next.RefreshSecretRef: {}}
	refs := make([]string, 0, len(previous.ObsoleteSecretRefs)+2)
	for _, ref := range append(append([]string(nil), previous.ObsoleteSecretRefs...), previous.AccessSecretRef, previous.RefreshSecretRef) {
		if ref == "" {
			continue
		}
		if _, exists := seen[ref]; exists {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

// cleanupObsoleteSecrets is best-effort after the new profile is committed.
// Failed deletions remain named by the authoritative profile and are retried
// by the next successful profile mutation. A cleanup failure must never make a
// committed login look uncommitted to its caller.
func (s ProfileStore) cleanupObsoleteSecrets(p Profile) {
	remaining := make([]string, 0, len(p.ObsoleteSecretRefs))
	for _, ref := range p.ObsoleteSecretRefs {
		if ref == p.AccessSecretRef || ref == p.RefreshSecretRef {
			remaining = append(remaining, ref)
			continue
		}
		if err := s.Secrets.Delete(ref); err != nil {
			remaining = append(remaining, ref)
		}
	}
	if len(remaining) == len(p.ObsoleteSecretRefs) {
		return
	}
	p.ObsoleteSecretRefs = remaining
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	// If publishing the reduced list fails, the previous profile still names
	// every cleanup candidate, including already-deleted idempotent entries.
	_ = s.writeActiveProfile(s.profilePath(p.Issuer), append(b, '\n'))
}

func pendingRefreshRef(issuer, cliClientSessionID string) string {
	return "revocation-" + pendingRevocationKey(issuer, cliClientSessionID) + "-refresh"
}

func (s ProfileStore) profilePath(issuer string) string {
	return filepath.Join(s.Path, "profiles", profileKey(issuer)+".json")
}

func (s ProfileStore) Load(issuer string) (Profile, error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return Profile{}, err
	}
	return s.loadNormalized(issuer)
}

func (s ProfileStore) loadNormalized(issuer string) (Profile, error) {
	b, err := os.ReadFile(s.profilePath(issuer))
	if credentialAbsenceOnly(err) {
		return Profile{}, ErrNoCredentials
	}
	if err != nil {
		return Profile{}, safeConfigCause("credential profile could not be read", err)
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return Profile{}, safeConfigCause("credential profile is invalid", err)
	}
	if p.Version != ProfileVersion || p.Issuer != issuer {
		return Profile{}, fmt.Errorf("unsupported or mismatched credential profile")
	}
	return p, nil
}

func (s ProfileStore) Save(p Profile, cred Credential) (resultErr error) {
	if err := validateCredential(cred); err != nil {
		return err
	}
	issuer, err := NormalizeIssuer(p.Issuer)
	if err != nil {
		return err
	}
	p.Issuer = issuer
	p.Version = ProfileVersion
	p.ObsoleteSecretRefs = nil
	p.UpdatedAt = time.Now().UTC()
	lock := newSharedLock(s.profilePath(issuer) + ".lock")
	if err := ensureProfileDirectory(filepath.Dir(s.profilePath(issuer))); err != nil {
		return err
	}
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	if err := s.recoverTransactionsLocked(issuer); err != nil {
		return err
	}
	if _, err := os.Stat(s.profilePath(issuer)); err == nil {
		return ErrProfileExists
	} else if !os.IsNotExist(err) {
		return err
	}
	p.AccessSecretRef, p.RefreshSecretRef, err = newSecretRefs(issuer)
	if err != nil {
		return err
	}
	tx, err := s.prepareNextProfileLocked("save", "", Profile{}, p, false)
	if err != nil {
		return err
	}
	if err := s.Secrets.Set(p.RefreshSecretRef, cred.RefreshToken); err != nil {
		return errors.Join(fmt.Errorf("store refresh token: %w", err), s.abortAuthTransactionLocked(tx))
	}
	rollback := func(cause error) error {
		cleanupErr := s.abortAuthTransactionLocked(tx)
		if cleanupErr != nil {
			return errors.Join(cause, fmt.Errorf("remove incomplete credentials: %w", cleanupErr))
		}
		return cause
	}
	if err := s.Secrets.Set(p.AccessSecretRef, cred.AccessToken); err != nil {
		return rollback(fmt.Errorf("store access token: %w", err))
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return rollback(err)
	}
	if err := s.writeActiveProfile(s.profilePath(p.Issuer), append(b, '\n')); err != nil {
		return rollback(err)
	}
	s.finishAuthTransactionLocked(tx, p)
	return nil
}

// Replace preserves the currently active profile if persisting its replacement
// fails. This is separate from refresh persistence because a rotated refresh
// token must never be rolled back to its server-invalid predecessor.
func (s ProfileStore) Replace(p Profile, cred Credential) (resultErr error) {
	if err := validateCredential(cred); err != nil {
		return err
	}
	issuer, err := NormalizeIssuer(p.Issuer)
	if err != nil {
		return err
	}
	p.Issuer = issuer
	p.Version = ProfileVersion
	p.UpdatedAt = time.Now().UTC()
	path := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	lock := newSharedLock(path + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	if err := s.recoverTransactionsLocked(issuer); err != nil {
		return err
	}
	previous, err := s.loadNormalized(issuer)
	if err != nil {
		return err
	}
	if _, err := s.credentialForProfile(previous); err != nil {
		return err
	}
	p.AccessSecretRef, p.RefreshSecretRef, err = newSecretRefs(issuer)
	if err != nil {
		return err
	}
	tx, err := s.prepareNextProfileLocked("replace", previous.CLIClientSessionID, previous, p, false)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		if rollbackErr := s.abortAuthTransactionLocked(tx); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("remove replacement credentials: %w", rollbackErr))
		}
		return cause
	}
	if err := s.Secrets.Set(p.RefreshSecretRef, cred.RefreshToken); err != nil {
		return errors.Join(fmt.Errorf("store replacement refresh token: %w", err), s.abortAuthTransactionLocked(tx))
	}
	if err := s.Secrets.Set(p.AccessSecretRef, cred.AccessToken); err != nil {
		return rollback(fmt.Errorf("store replacement access token: %w", err))
	}
	p.ObsoleteSecretRefs = obsoleteSecretRefs(previous, p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return rollback(err)
	}
	if err := s.writeActiveProfile(path, append(b, '\n')); err != nil {
		return rollback(err)
	}
	s.finishAuthTransactionLocked(tx, p)
	return nil
}

// Switch compares, retains, and replaces the active session under one issuer
// lock so overlapping account switches cannot associate credentials with a
// stale client session ID. Replaying the current session replaces its
// credentials without queuing that same active session for revocation.
func (s ProfileStore) Switch(expectedSessionID string, p Profile, cred Credential) (resultErr error) {
	if err := validateCredential(cred); err != nil {
		return err
	}
	issuer, err := NormalizeIssuer(p.Issuer)
	if err != nil {
		return err
	}
	p.Issuer = issuer
	p.Version = ProfileVersion
	p.UpdatedAt = time.Now().UTC()
	path := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	lock := newSharedLock(path + ".lock")
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	if err := s.recoverTransactionsLocked(issuer); err != nil {
		return err
	}
	previous, err := s.loadNormalized(issuer)
	if err != nil {
		return err
	}
	if previous.CLIClientSessionID != expectedSessionID {
		return ErrProfileChanged
	}
	// A replay of the active session is a refresh, not an account switch.
	// Keep its references stable and write the rotated refresh value first so a
	// crash cannot restore a server-invalid predecessor or enqueue the active
	// session for revocation.
	if previous.CLIClientSessionID == p.CLIClientSessionID {
		p.AccessSecretRef = previous.AccessSecretRef
		p.RefreshSecretRef = previous.RefreshSecretRef
		p.ObsoleteSecretRefs = previous.ObsoleteSecretRefs
		if err := s.saveLocked(p, cred); err != nil {
			return err
		}
		s.cleanupObsoleteSecrets(p)
		return nil
	}
	previousCred, err := s.credentialForProfile(previous)
	if err != nil {
		return err
	}
	queuePrevious := previous.CLIClientSessionID != p.CLIClientSessionID
	p.AccessSecretRef, p.RefreshSecretRef, err = newSecretRefs(issuer)
	if err != nil {
		return err
	}
	tx, err := s.prepareNextProfileLocked("switch", expectedSessionID, previous, p, queuePrevious)
	if err != nil {
		return err
	}
	if queuePrevious {
		accountID := previous.Account.ID
		if accountID != "" && !validCredentialID(accountID) {
			accountID = ""
		}
		if err := s.queueRevocationLocked(issuer, previous.CLIClientSessionID, previousCred.RefreshToken, accountID); err != nil {
			return errors.Join(err, s.abortAuthTransactionLocked(tx))
		}
	}
	rollback := func(cause error) error {
		return errors.Join(cause, s.abortAuthTransactionLocked(tx))
	}
	if err := s.Secrets.Set(p.RefreshSecretRef, cred.RefreshToken); err != nil {
		return rollback(err)
	}
	if err := s.Secrets.Set(p.AccessSecretRef, cred.AccessToken); err != nil {
		return rollback(err)
	}
	p.ObsoleteSecretRefs = obsoleteSecretRefs(previous, p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return rollback(err)
	}
	if err := s.writeActiveProfile(path, append(b, '\n')); err != nil {
		return rollback(err)
	}
	s.finishAuthTransactionLocked(tx, p)
	return nil
}

func (s ProfileStore) saveLocked(p Profile, cred Credential) error {
	if err := validateCredential(cred); err != nil {
		return err
	}
	// The durable original-bearer attempt was prepared before HTTP rotation.
	// Replace refresh first, then access and profile metadata; an interruption
	// resumes that exact server receipt until the complete pair is committed.
	if err := s.Secrets.Set(p.RefreshSecretRef, cred.RefreshToken); err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}
	if err := s.Secrets.Set(p.AccessSecretRef, cred.AccessToken); err != nil {
		return fmt.Errorf("store access token: %w", err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return s.writeActiveProfile(s.profilePath(p.Issuer), append(b, '\n'))
}

func (s ProfileStore) Delete(issuer string) error {
	_, err := s.Remove(issuer)
	if errors.Is(err, ErrNoCredentials) {
		return nil
	}
	return err
}

// Remove atomically reads and deletes a profile and its secrets. The returned
// refresh credential lets logout revoke remotely after local cleanup succeeds.
func (s ProfileStore) Remove(issuer string) (Credential, error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return Credential{}, err
	}
	path := s.profilePath(issuer)
	if err := ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return Credential{}, err
	}
	lock := newSharedLock(path + ".lock")
	return s.removeCredential(issuer, lock)
}

func (s ProfileStore) removeCredential(issuer string, lock credentialLock) (credential Credential, resultErr error) {
	if issuer == "" || lock == nil {
		return Credential{}, ErrCredentialStoreUnavailable
	}
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
	if errors.Is(err, ErrNoCredentials) {
		return Credential{}, ErrNoCredentials
	}
	if err != nil {
		return Credential{}, err
	}
	cred, credentialErr := s.credentialForProfile(p)
	var errs []error
	if attempt, err := s.loadRefreshAttempt(issuer); err == nil {
		errs = append(errs, s.clearRefreshAttempt(issuer, attempt))
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	errs = append(errs, s.Secrets.Delete(p.AccessSecretRef), s.Secrets.Delete(p.RefreshSecretRef), s.DeleteManagedSSHIdentity(p.Issuer, p.CLIClientSessionID), s.DeletePeerEndpointIdentity(p.Issuer, p.CLIClientSessionID), s.DeletePeerAccountRoot(p.Issuer, p.Account.ID))
	for _, ref := range p.ObsoleteSecretRefs {
		errs = append(errs, s.Secrets.Delete(ref))
	}
	if err := os.Remove(s.profilePath(p.Issuer)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Credential{}, err
	}
	return cred, credentialErr
}

func ProfileStoreFor(cfg *Config) (ProfileStore, error) {
	defaultDir, err := DefaultCredentialDir()
	if err != nil {
		return ProfileStore{}, err
	}
	dir := cfg.Auth.ProfileDir
	if dir == "" {
		dir = defaultDir
	} else if !filepath.IsAbs(dir) {
		return ProfileStore{}, errors.New("auth.profile_dir must be an absolute path")
	}
	dir = filepath.Clean(dir)
	if err := publishCredentialLocation(defaultDir, dir); err != nil {
		return ProfileStore{}, err
	}
	var secrets SecretStore = KeyringStore{}
	if cfg.Auth.AllowFileFallback {
		secrets = FileSecretStore{Dir: filepath.Join(dir, "secrets")}
	}
	return ProfileStore{Path: dir, Secrets: secrets}, nil
}

func publishCredentialLocation(defaultDir, dir string) error {
	location := struct {
		Version    int    `json:"version"`
		ProfileDir string `json:"profile_dir"`
	}{Version: ProfileVersion, ProfileDir: dir}
	b, err := json.MarshalIndent(location, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(defaultDir), "credentials-location.json")
	b = append(b, '\n')
	if current, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(current, b) {
		return nil // every command opens the store; rewriting (two fsyncs) an unchanged file only costs time
	}
	if err := atomicWrite(path, b, 0o600); err != nil {
		return fmt.Errorf("publish shared credential location: %w", err)
	}
	return nil
}
func DefaultCredentialDir() (string, error) {
	return userpaths.Config("paperboat/credentials")
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, data, atomicfile.Options{Mode: mode, OwnerUID: -1, OwnerGID: -1})
}
