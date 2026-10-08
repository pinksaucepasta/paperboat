//go:build darwin || linux

package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// macOS security's interactive command is limited to4096 bytes. Keep one small
// wrapping key in Keychain and the bounded authenticated ciphertext in an atomic
// owner-only file; never pass the vault record to security or a command argument.
type keychainVaultStore struct {
	directory string
	keys      SecretStore
}

func keychainVaultReference(ref string) bool {
	const prefix = "environment-password-vault-v1-"
	if !strings.HasPrefix(ref, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(ref, prefix)
	if len(suffix) != 32 || suffix != strings.ToLower(suffix) {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}
func (s keychainVaultStore) lock(ref string) (string, func() error, error) {
	if !keychainVaultReference(ref) || s.keys == nil || !filepath.IsAbs(s.directory) {
		return "", nil, ErrCredentialStoreUnavailable
	}
	if err := os.MkdirAll(s.directory, 0700); err != nil {
		return "", nil, err
	}
	if err := validateCredentialDirectory(s.directory); err != nil {
		return "", nil, err
	}
	path := filepath.Join(s.directory, ref+".sealed")
	lock := newSharedLock(path + ".lock")
	if err := lock.Lock(); err != nil {
		return "", nil, err
	}
	return path, lock.Unlock, nil
}
func keychainVaultAAD(ref string) []byte {
	return []byte("paperboat.environment.local-vault/v1\x00" + ref)
}
func keychainVaultAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrCredentialStoreUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s keychainVaultStore) wrappingKey(ref string, create bool) ([]byte, error) {
	value, err := s.keys.Get(ref + "-wrapping-key")
	if credentialAbsenceOnly(err) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			clear(key)
			return nil, err
		}
		if err := s.keys.Set(ref+"-wrapping-key", base64.RawURLEncoding.EncodeToString(key)); err != nil {
			clear(key)
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, ErrCredentialStoreUnavailable
	}
	return key, nil
}
func (s keychainVaultStore) Set(ref, value string) (resultErr error) {
	if len(value) == 0 || len(value) > passwordVaultRecordBytes {
		return ErrCredentialStoreUnavailable
	}
	path, unlock, err := s.lock(ref)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	_, existingErr := readKeychainVaultFile(path)
	if existingErr != nil && !credentialAbsenceOnly(existingErr) {
		return safeConfigCause("environment vault file could not be inspected", existingErr)
	}
	key, err := s.wrappingKey(ref, credentialAbsenceOnly(existingErr))
	if err != nil {
		return credentialStoreFailure("environment vault wrapping key is unavailable", err)
	}
	defer clear(key)
	aead, err := keychainVaultAEAD(key)
	if err != nil {
		return safeConfigCause("environment vault encryption could not be initialized", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return safeConfigCause("environment vault encryption could not be initialized", err)
	}
	plain := []byte(value)
	defer clear(plain)
	raw := append([]byte{1}, nonce...)
	raw = aead.Seal(raw, nonce, plain, keychainVaultAAD(ref))
	if err := writeCredentialFile(path, raw); err != nil {
		return credentialStoreFailure("environment vault could not be stored", err)
	}
	return nil
}
func (s keychainVaultStore) Get(ref string) (value string, resultErr error) {
	path, unlock, err := s.lock(ref)
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, unlock())
		if resultErr != nil {
			value = ""
		}
	}()
	raw, err := readKeychainVaultFile(path)
	if credentialAbsenceOnly(err) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", safeConfigCause("environment vault file could not be read", err)
	}
	defer clear(raw)
	key, err := s.wrappingKey(ref, false)
	if err != nil {
		return "", credentialStoreFailure("environment vault wrapping key is unavailable", err)
	}
	defer clear(key)
	aead, err := keychainVaultAEAD(key)
	if err != nil {
		return "", safeConfigCause("environment vault encryption could not be initialized", err)
	}
	if len(raw) <= 1+aead.NonceSize()+aead.Overhead() || raw[0] != 1 {
		return "", ErrCredentialStoreUnavailable
	}
	plain, err := aead.Open(nil, raw[1:13], raw[13:], keychainVaultAAD(ref))
	if err != nil {
		return "", credentialStoreFailure("environment vault authentication failed", err)
	}
	defer clear(plain)
	if len(plain) == 0 || len(plain) > passwordVaultRecordBytes {
		return "", ErrCredentialStoreUnavailable
	}
	return string(plain), nil
}
func (s keychainVaultStore) Delete(ref string) (resultErr error) {
	path, unlock, err := s.lock(ref)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	if _, err := readKeychainVaultFile(path); err != nil && !credentialAbsenceOnly(err) {
		return safeConfigCause("environment vault file could not be inspected", err)
	}
	if err := os.Remove(path); err != nil && !credentialAbsenceOnly(err) {
		return safeConfigCause("environment vault file could not be removed", err)
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return safeConfigCause("environment vault directory could not be synchronized", err)
	}
	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		return safeConfigCause("environment vault directory could not be synchronized", err)
	}
	if err := s.keys.Delete(ref + "-wrapping-key"); err != nil && !credentialAbsenceOnly(err) {
		return safeConfigCause("environment vault wrapping key could not be removed", err)
	}
	return nil
}
func readKeychainVaultFile(path string) (raw []byte, resultErr error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, safeConfigCause("environment vault file could not be closed", closeErr))
		}
		if resultErr != nil {
			clear(raw)
			raw = nil
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > int64(passwordVaultRecordBytes+29) {
		return nil, ErrCredentialStoreUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(file, int64(passwordVaultRecordBytes+30)))
	if err != nil {
		return nil, safeConfigCause("environment vault file could not be read", err)
	}
	if len(raw) > passwordVaultRecordBytes+29 {
		return nil, ErrCredentialStoreUnavailable
	}
	return raw, nil
}
