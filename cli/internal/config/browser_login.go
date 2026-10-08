package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// BrowserLoginState is private durable state for one server-bound approval attempt.
// The device code never appears in terminal output or an approval URL.
type BrowserLoginState struct {
	Version           int         `json:"version"`
	Issuer            string      `json:"issuer"`
	DeviceCode        string      `json:"device_code"`
	ApprovalURL       string      `json:"approval_url"`
	ExpiresAt         time.Time   `json:"expires_at"`
	Interval          int         `json:"interval"`
	ExpectedAccountID string      `json:"expected_account_id,omitempty"`
	PreviousSessionID string      `json:"previous_session_id,omitempty"`
	Cancelled         bool        `json:"cancelled,omitempty"`
	Profile           *Profile    `json:"profile,omitempty"`
	Credential        *Credential `json:"credential,omitempty"`
}

func (s ProfileStore) WithBrowserLogin(issuer string, fn func(*BrowserLoginState, func() error) error) (resultErr error) {
	if s.Path == "" || s.Secrets == nil {
		return ErrCredentialStoreUnavailable
	}
	normalized, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	path := s.profilePath(normalized)
	if err = ensureProfileDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	lock := newSharedLock(path + ".browser-login.lock")
	if err = lock.Lock(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	ref := "browser-login-v1-" + profileKey(normalized+"\x00"+filepath.Clean(s.Path))
	var state BrowserLoginState
	encoded, err := s.Secrets.Get(ref)
	if err != nil && !credentialAbsenceOnly(err) {
		return safeConfigCause("pending login state could not be loaded", err)
	}
	if err == nil {
		if len(encoded) > 4<<10 {
			return errors.New("pending login exceeds storage limit")
		}
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&state); err != nil {
			return safeConfigCause("pending login is invalid", err)
		}
		b, e := json.Marshal(state)
		if e != nil {
			return safeConfigCause("pending login state could not be validated", e)
		}
		if string(b) != encoded {
			return errors.New("pending login is invalid")
		}
		if err = validateBrowserLogin(state, normalized); err != nil {
			return err
		}
	}
	save := func() error {
		if state == (BrowserLoginState{}) {
			e := s.Secrets.Delete(ref)
			if e == nil || credentialAbsenceOnly(e) {
				return nil
			}
			return safeConfigCause("pending login state could not be removed", e)
		}
		if err := validateBrowserLogin(state, normalized); err != nil {
			return err
		}
		b, err := json.Marshal(state)
		if err != nil {
			return safeConfigCause("pending login state could not be encoded", err)
		}
		if len(b) > 4<<10 {
			return errors.New("pending login exceeds storage limit")
		}
		if err := s.Secrets.Set(ref, string(b)); err != nil {
			return safeConfigCause("pending login state could not be stored", err)
		}
		return nil
	}
	return fn(&state, save)
}
func validateBrowserLogin(s BrowserLoginState, issuer string) error {
	code, err := base64.RawURLEncoding.Strict().DecodeString(s.DeviceCode)
	if err != nil {
		return safeConfigCause("pending login is invalid", err)
	}
	defer clear(code)
	if len(code) != 32 || base64.RawURLEncoding.EncodeToString(code) != s.DeviceCode || s.Version != 1 || s.Issuer != issuer || s.ExpiresAt.IsZero() || s.Interval < 1 || s.Interval > 60 || len(s.ApprovalURL) > 2048 {
		return errors.New("pending login is invalid")
	}
	if (s.Profile == nil) != (s.Credential == nil) {
		return errors.New("pending login response is incomplete")
	}
	if s.Profile != nil {
		if s.Profile.Issuer != issuer || !validCredentialID(s.Profile.CLIClientSessionID) || s.Profile.Account.ID == "" || s.Profile.AccessExpiresAt.IsZero() || !s.Profile.AccessExpiresAt.Equal(s.Credential.ExpiresAt) || s.Credential.TokenType != "Bearer" {
			return errors.New("pending login response is invalid")
		}
		if err := validateCredential(*s.Credential); err != nil {
			return errors.New("pending login credential is invalid")
		}
	}
	return nil
}
