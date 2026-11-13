//go:build windows

package service

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"os"
	"path/filepath"
	"strings"
)

// OwnedWindowsExecutable reads the existing authoritative service declaration.
// It preserves an immutable owner image when the feature Source.Version moves.
func OwnedWindowsExecutable(definitionPath string) (string, error) {
	if !filepath.IsAbs(definitionPath) || filepath.Ext(definitionPath) != ".json" {
		return "", ErrInvalidDefinition
	}
	// An absent declaration is ordinary cold lifecycle state. Validate every
	// existing ancestor before reporting absence so unsafe paths cannot grant
	// recovery. Present declarations retain the complete ownership policy below.
	if err := validateWindowsAncestorDirectories(definitionPath, true); err != nil {
		return "", err
	}
	if _, err := os.Lstat(definitionPath); err != nil {
		return "", err
	}
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
	if err := installsource.VerifyDigest(path, identity.Length, identity.SHA256); err != nil {
		if errors.Is(err, installsource.ErrInvalid) {
			err = ErrInvalidDefinition
		}
		return WindowsExecutableIdentity{}, err
	}
	return identity, nil
}
