//go:build darwin

package config

import (
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

type KeyringStore struct{}

func (KeyringStore) Set(ref, value string) error {
	if keychainVaultReference(ref) {
		store, err := macOSVaultStore()
		if err != nil {
			return err
		}
		return store.Set(ref, value)
	}
	return vaultKeychainError(keychainSet(ref, value))
}
func (KeyringStore) Get(ref string) (string, error) {
	if keychainVaultReference(ref) {
		store, err := macOSVaultStore()
		if err != nil {
			return "", err
		}
		return store.Get(ref)
	}
	value, err := keychainGet(ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	return value, vaultKeychainError(err)
}
func (KeyringStore) Delete(ref string) error {
	if keychainVaultReference(ref) {
		store, err := macOSVaultStore()
		if err != nil {
			return err
		}
		return store.Delete(ref)
	}
	err := keychainDelete(ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return vaultKeychainError(err)
}

func macOSVaultStore() (keychainVaultStore, error) {
	directory, err := DefaultCredentialDir()
	return keychainVaultStore{directory: filepath.Join(directory, "environment-vaults"), keys: smallKeychainStore{}}, err
}

type smallKeychainStore struct{}

func (smallKeychainStore) Set(ref, value string) error {
	return vaultKeychainError(keychainSet(ref, value))
}
func (smallKeychainStore) Get(ref string) (string, error) {
	value, err := keychainGet(ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	return value, vaultKeychainError(err)
}
func (smallKeychainStore) Delete(ref string) error {
	err := keychainDelete(ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return vaultKeychainError(err)
}

func keychainSet(ref, value string) error {
	_, err := boundedKeychain(func() (struct{}, error) { return struct{}{}, keyring.Set(keyringService, ref, value) })
	return err
}

func keychainGet(ref string) (string, error) {
	return boundedKeychain(func() (string, error) { return keyring.Get(keyringService, ref) })
}

func keychainDelete(ref string) error {
	_, err := boundedKeychain(func() (struct{}, error) { return struct{}{}, keyring.Delete(keyringService, ref) })
	return err
}

func vaultKeychainError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCredentialStoreUnavailable) { // already worded by boundedKeychain
		return err
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 36 {
		return credentialStoreFailure("macOS Keychain does not allow access from this session; unlock the login keychain and run the vault command from the signed-in desktop session", err)
	}
	return credentialStoreFailure("macOS Keychain request failed", err)
}
