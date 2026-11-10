//go:build windows

package config

import (
	"encoding/hex"
	"fmt"
	"strings"
)

const windowsCredentialBlobMaxBytes = 5120

type KeyringStore struct{}

// Only the canonical hashed ENV vault namespace permits larger DPAPI records.
func windowsSecretMaxBytes(ref string) int {
	const prefix = "environment-password-vault-v1-"
	suffix := strings.TrimPrefix(ref, prefix)
	if strings.HasPrefix(ref, prefix) && len(suffix) == 32 && suffix == strings.ToLower(suffix) {
		if _, err := hex.DecodeString(suffix); err == nil {
			return passwordVaultRecordBytes
		}
	}
	return windowsCredentialBlobMaxBytes
}

// DPAPI (machine scope, one file per reference) is the only credential store: Set replaces the file atomically
// and every logon type (interactive, scheduled task, S4U owner workload) resolves the same value.
func (KeyringStore) Set(ref, value string) error {
	if value == "" {
		return fmt.Errorf("%w: refusing to store an empty credential", ErrCredentialStoreUnavailable)
	}
	if len(value) > windowsSecretMaxBytes(ref) {
		return fmt.Errorf("%w: credential exceeds %d bytes", ErrCredentialStoreUnavailable, windowsSecretMaxBytes(ref))
	}
	if err := setDPAPISecret(ref, value); err != nil {
		return credentialStoreFailure("OS credential store unavailable", err)
	}
	return nil
}

func (KeyringStore) Get(ref string) (value string, resultErr error) {
	defer func() {
		if resultErr == nil || credentialAbsenceOnly(resultErr) {
			return
		}
		resultErr = credentialStoreFailure("OS credential store unavailable", resultErr)
		value = ""
	}()
	value, err := getDPAPISecret(ref)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%w: DPAPI credential is empty", ErrCredentialStoreUnavailable)
	}
	return value, nil
}

func (KeyringStore) Delete(ref string) error {
	if err := deleteDPAPISecret(ref); err != nil {
		return credentialStoreFailure("OS credential store unavailable", err)
	}
	return nil
}
