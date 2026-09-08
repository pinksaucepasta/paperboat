//go:build darwin

package config

import (
	"errors"
	"fmt"
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
	return keyring.Set(keyringService, ref, value)
}
func (KeyringStore) Get(ref string) (string, error) {
	if keychainVaultReference(ref) {
		store, err := macOSVaultStore()
		if err != nil {
			return "", err
		}
		return store.Get(ref)
	}
	value, err := keyring.Get(keyringService, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	return value, err
}
func (KeyringStore) Delete(ref string) error {
	if keychainVaultReference(ref) {
		store, err := macOSVaultStore()
		if err != nil {
			return err
		}
		return store.Delete(ref)
	}
	err := keyring.Delete(keyringService, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func macOSVaultStore() (keychainVaultStore, error) {
	directory, err := DefaultCredentialDir()
	return keychainVaultStore{directory: filepath.Join(directory, "environment-vaults"), keys: smallKeychainStore{}}, err
}

type smallKeychainStore struct{}

func (smallKeychainStore) Set(ref, value string) error {
	return vaultKeychainError(keyring.Set(keyringService, ref, value))
}
func (smallKeychainStore) Get(ref string) (string, error) {
	value, err := keyring.Get(keyringService, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	return value, vaultKeychainError(err)
}
func (smallKeychainStore) Delete(ref string) error {
	err := keyring.Delete(keyringService, ref)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return vaultKeychainError(err)
}

func vaultKeychainError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 36 {
		return fmt.Errorf("macOS Keychain does not allow access from this session; unlock the login keychain and run the vault command from the signed-in desktop session: %w", ErrCredentialStoreUnavailable)
	}
	return err
}
