//go:build windows

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// OwnedWindowsExecutable reads the existing authoritative service declaration.
// It preserves an immutable owner image when the feature Source.Version moves.
func OwnedWindowsExecutable(definitionPath string) (string, error) {
	definition, err := readWindowsServiceDefinitionForRemoval(definitionPath)
	if err != nil {
		return "", err
	}
	return definition.Executable, nil
}

type WindowsExecutableIdentity struct {
	Executable, SHA256 string
	Length             int64
}

func OwnedWindowsRoleExecutable(kind, instance string) (WindowsExecutableIdentity, error) {
	path := filepath.Join(windowsServiceDefinitionRoot, windowsServiceName(kind, instance)+".json")
	definition, err := readWindowsServiceDefinition(path)
	if err != nil {
		return WindowsExecutableIdentity{}, err
	}
	if !windowssecurity.ProtectedDACLMatches(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)") || len(definition.ExecutableSHA256) != 64 || definition.ExecutableLength < 1 || definition.ExecutableLength > 256<<20 {
		return WindowsExecutableIdentity{}, ErrInvalidDefinition
	}
	return WindowsExecutableIdentity{Executable: definition.Executable, SHA256: definition.ExecutableSHA256, Length: definition.ExecutableLength}, nil
}
func VerifyOwnedWindowsRoleExecutable(kind, instance, path string) (WindowsExecutableIdentity, error) {
	identity, err := OwnedWindowsRoleExecutable(kind, instance)
	if err != nil || !strings.EqualFold(identity.Executable, path) {
		return WindowsExecutableIdentity{}, ErrInvalidDefinition
	}
	file, err := os.Open(path)
	if err != nil {
		return WindowsExecutableIdentity{}, err
	}
	defer file.Close()
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, identity.Length+1))
	if err != nil || n != identity.Length || hex.EncodeToString(digest.Sum(nil)) != identity.SHA256 {
		return WindowsExecutableIdentity{}, ErrInvalidDefinition
	}
	return identity, nil
}
