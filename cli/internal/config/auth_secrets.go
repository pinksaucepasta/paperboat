package config

import (
	"errors"
	"os"
	"path/filepath"
)

type SecretStore interface {
	Set(ref, value string) error
	Get(ref string) (string, error)
	Delete(ref string) error
}

// PurgeCredentialStore removes the complete production Paperboat credential
// namespace. A product uninstall cannot depend only on profile indexes: an
// interrupted older uninstall may already have removed those indexes while
// leaving OS-backed secrets behind.
func PurgeCredentialStore() error { return purgeCredentialStore() }

type FileSecretStore struct{ Dir string }

func (s FileSecretStore) path(ref string) string { return filepath.Join(s.Dir, ref+".secret") }

func (s FileSecretStore) Set(ref, value string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return safeConfigCause("credential storage directory could not be created", err)
	}
	if err := validateCredentialDirectory(s.Dir); err != nil {
		if isSafeConfigError(err) {
			return err
		}
		return safeConfigCause("credential storage directory could not be validated", err)
	}
	if err := writeCredentialFile(s.path(ref), []byte(value)); err != nil {
		if isSafeConfigError(err) {
			return err
		}
		return safeConfigCause("credential file could not be stored", err)
	}
	return nil
}

func (s FileSecretStore) Get(ref string) (string, error) {
	if err := validateCredentialDirectory(s.Dir); err != nil {
		if credentialAbsenceOnly(err) {
			return "", ErrSecretNotFound
		}
		if isSafeConfigError(err) {
			return "", err
		}
		return "", safeConfigCause("credential storage directory could not be validated", err)
	}
	b, err := readCredentialFile(s.path(ref))
	if credentialAbsenceOnly(err) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		if errors.Is(err, ErrCredentialRequiresInteractiveLogin) || isSafeConfigError(err) {
			return "", err
		}
		return "", safeConfigCause("credential file could not be read", err)
	}
	return string(b), nil
}

func (s FileSecretStore) Delete(ref string) error {
	if err := validateCredentialDirectory(s.Dir); err != nil {
		if credentialAbsenceOnly(err) {
			return nil
		}
		if isSafeConfigError(err) {
			return err
		}
		return safeConfigCause("credential storage directory could not be validated", err)
	}
	err := os.Remove(s.path(ref))
	if credentialAbsenceOnly(err) {
		return nil
	}
	if err != nil {
		if isSafeConfigError(err) {
			return err
		}
		return safeConfigCause("credential file could not be deleted", err)
	}
	return nil
}

type AuthSource interface{ Credential() (Credential, error) }

type NoCredentialsSource struct{}

func (NoCredentialsSource) Credential() (Credential, error) {
	return Credential{}, ErrNoCredentials
}
