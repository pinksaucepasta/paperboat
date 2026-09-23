package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	enrollmentLoginRecordVersion  = 1
	enrollmentLoginRecordKind     = "enrollment_login"
	enrollmentLoginTokenLength    = 26
	enrollmentLoginVerifierBytes  = 32
	enrollmentLoginVerifierLength = 43
	// The response contains two bearer tokens and a small profile. Keep the
	// recovery record within the existing 5 KiB generic credential limit used
	// by Windows while leaving room for normal account metadata.
	enrollmentLoginMaxEncodedBytes = 4 << 10
)

// EnrollmentLoginState is the durable state for one token login attempt.
// Token and Verifier are retained before the network exchange; Profile and
// Credential are retained after the server issues the CLI session. Cancelled
// prevents a logout racing an in-flight login from silently resuming sign-in.
type EnrollmentLoginState struct {
	Token      string
	Verifier   string
	Cancelled  bool
	Profile    *Profile
	Credential *Credential
}

type enrollmentLoginCredential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type enrollmentLoginRecord struct {
	Version    int                        `json:"version"`
	Kind       string                     `json:"kind"`
	Issuer     string                     `json:"issuer"`
	Token      string                     `json:"token,omitempty"`
	Verifier   string                     `json:"verifier,omitempty"`
	Cancelled  bool                       `json:"cancelled,omitempty"`
	Profile    *Profile                   `json:"profile,omitempty"`
	Credential *enrollmentLoginCredential `json:"credential,omitempty"`
}

// WithEnrollmentLogin loads the recovery state for issuer, holds the
// issuer-specific enrollment-login lock while fn runs, and exposes save for
// the callback's explicit recovery checkpoints. The profile lock is acquired
// only by callback operations such as Save or Switch, so those operations do
// not deadlock with this lock.
func (s ProfileStore) WithEnrollmentLogin(issuer string, fn func(state *EnrollmentLoginState, save func() error) error) (resultErr error) {
	if s.Path == "" || s.Secrets == nil {
		return ErrCredentialStoreUnavailable
	}
	if fn == nil {
		return errors.New("enrollment login callback is required")
	}
	normalized, err := NormalizeIssuer(issuer)
	if err != nil {
		return err
	}
	profilePath := s.profilePath(normalized)
	if err := ensureProfileDirectory(filepath.Dir(profilePath)); err != nil {
		return fmt.Errorf("prepare enrollment login storage: %w", err)
	}
	lock := newSharedLock(profilePath + ".enrollment-login.lock")
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock enrollment login recovery: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Unlock())
	}()

	state, err := loadEnrollmentLoginState(s.Secrets, enrollmentLoginSecretRef(normalized), normalized)
	if err != nil {
		return err
	}
	save := func() error {
		return saveEnrollmentLoginState(s.Secrets, enrollmentLoginSecretRef(normalized), normalized, state)
	}
	return fn(&state, save)
}

func enrollmentLoginSecretRef(issuer string) string {
	return "enrollment-login-v1-" + profileKey(issuer)
}

func loadEnrollmentLoginState(store SecretStore, ref, issuer string) (EnrollmentLoginState, error) {
	encoded, err := store.Get(ref)
	if errors.Is(err, ErrSecretNotFound) || errors.Is(err, os.ErrNotExist) {
		return EnrollmentLoginState{}, nil
	}
	if err != nil {
		return EnrollmentLoginState{}, fmt.Errorf("load enrollment login recovery: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > enrollmentLoginMaxEncodedBytes {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	}
	var record enrollmentLoginRecord
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	} else if !errors.Is(err, io.EOF) {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	}
	canonical, err := json.Marshal(record)
	if err != nil || string(canonical) != encoded {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	}
	state, err := enrollmentLoginStateFromRecord(record, issuer)
	if err != nil {
		return EnrollmentLoginState{}, err
	}
	return state, nil
}

func saveEnrollmentLoginState(store SecretStore, ref, issuer string, state EnrollmentLoginState) error {
	if state == (EnrollmentLoginState{}) {
		if err := store.Delete(ref); err != nil && !errors.Is(err, ErrSecretNotFound) && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear enrollment login recovery: %w", err)
		}
		return nil
	}
	record, err := enrollmentLoginRecordFromState(state, issuer)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return errors.New("encode enrollment login recovery record")
	}
	if len(encoded) > enrollmentLoginMaxEncodedBytes {
		return errors.New("enrollment login recovery record is too large")
	}
	if err := store.Set(ref, string(encoded)); err != nil {
		return fmt.Errorf("store enrollment login recovery: %w", err)
	}
	return nil
}

func enrollmentLoginRecordFromState(state EnrollmentLoginState, issuer string) (enrollmentLoginRecord, error) {
	if err := validateEnrollmentLoginState(state, issuer); err != nil {
		return enrollmentLoginRecord{}, err
	}
	record := enrollmentLoginRecord{
		Version:   enrollmentLoginRecordVersion,
		Kind:      enrollmentLoginRecordKind,
		Issuer:    issuer,
		Token:     state.Token,
		Verifier:  state.Verifier,
		Cancelled: state.Cancelled,
	}
	if state.Profile != nil {
		profile := *state.Profile
		profile.Issuer = issuer
		record.Profile = &profile
		credential := *state.Credential
		record.Credential = &enrollmentLoginCredential{
			AccessToken:  credential.AccessToken,
			RefreshToken: credential.RefreshToken,
			TokenType:    credential.TokenType,
			ExpiresAt:    credential.ExpiresAt,
		}
	}
	return record, nil
}

func enrollmentLoginStateFromRecord(record enrollmentLoginRecord, issuer string) (EnrollmentLoginState, error) {
	if record.Version != enrollmentLoginRecordVersion || record.Kind != enrollmentLoginRecordKind || record.Issuer != issuer {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	}
	state := EnrollmentLoginState{Token: record.Token, Verifier: record.Verifier, Cancelled: record.Cancelled}
	if record.Profile != nil {
		if record.Credential == nil {
			return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
		}
		profile := *record.Profile
		credential := &Credential{
			AccessToken:  record.Credential.AccessToken,
			RefreshToken: record.Credential.RefreshToken,
			TokenType:    record.Credential.TokenType,
			ExpiresAt:    record.Credential.ExpiresAt,
		}
		state.Profile = &profile
		state.Credential = credential
	} else if record.Credential != nil {
		return EnrollmentLoginState{}, errors.New("enrollment login recovery record is invalid")
	}
	if err := validateEnrollmentLoginState(state, issuer); err != nil {
		return EnrollmentLoginState{}, err
	}
	return state, nil
}

func validateEnrollmentLoginState(state EnrollmentLoginState, issuer string) error {
	tokenPresent, verifierPresent := state.Token != "", state.Verifier != ""
	if tokenPresent != verifierPresent {
		return errors.New("enrollment login recovery token and verifier must be provided together")
	}
	if tokenPresent {
		if !validEnrollmentLoginToken(state.Token) {
			return errors.New("enrollment login recovery token is invalid")
		}
		if !validEnrollmentLoginVerifier(state.Verifier) {
			return errors.New("enrollment login recovery verifier is invalid")
		}
	}
	if (state.Profile == nil) != (state.Credential == nil) {
		return errors.New("enrollment login recovery profile and credential must be provided together")
	}
	if state.Profile == nil {
		if !tokenPresent {
			if state.Cancelled {
				return errors.New("enrollment login cancellation has no recoverable state")
			}
			return errors.New("enrollment login recovery state is empty")
		}
		return nil
	}
	profileIssuer, err := NormalizeIssuer(state.Profile.Issuer)
	if err != nil || profileIssuer != issuer {
		return errors.New("enrollment login recovery profile issuer does not match")
	}
	if !validCredentialID(state.Profile.CLIClientSessionID) || strings.TrimSpace(state.Profile.Account.ID) == "" {
		return errors.New("enrollment login recovery profile is incomplete")
	}
	if state.Profile.AccessExpiresAt.IsZero() || state.Credential.ExpiresAt.IsZero() || !state.Profile.AccessExpiresAt.Equal(state.Credential.ExpiresAt) || state.Credential.TokenType != "Bearer" {
		return errors.New("enrollment login recovery credential expiry is invalid")
	}
	if err := validateCredential(*state.Credential); err != nil {
		return errors.New("enrollment login recovery credential is invalid")
	}
	return nil
}

func validEnrollmentLoginToken(token string) bool {
	if len(token) != enrollmentLoginTokenLength || strings.TrimSpace(token) != token {
		return false
	}
	for i := range token {
		if !((token[i] >= 'A' && token[i] <= 'Z') || (token[i] >= '0' && token[i] <= '9')) {
			return false
		}
	}
	return true
}

func validEnrollmentLoginVerifier(verifier string) bool {
	if len(verifier) != enrollmentLoginVerifierLength || strings.TrimSpace(verifier) != verifier {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(verifier)
	return err == nil && len(decoded) == enrollmentLoginVerifierBytes && base64.RawURLEncoding.EncodeToString(decoded) == verifier
}
