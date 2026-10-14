//go:build windows

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These checks touch only test-owned files and never connect to SCM.
func TestWindowsServiceExecutableOwnershipAcceptsOnlyImmutableNativePins(t *testing.T) {
	layout, err := WindowsUserLayout("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(layout.ReleasesRoot, "versions", "2026.10.08.20", "pb.exe")
	for _, path := range []string{layout.Binary, pin} {
		if !windowsServiceExecutableOwned(layout, path) {
			t.Fatalf("owned executable rejected: %q", path)
		}
	}
	for _, path := range []string{
		filepath.Join(layout.ReleasesRoot, "versions", "pb.exe"),
		filepath.Join(layout.ReleasesRoot, "versions", "2026.10.08.20", "other.exe"),
		filepath.Join(layout.ReleasesRoot, "versions", "2026.10.08.20", "nested", "pb.exe"),
		filepath.Join(filepath.Dir(layout.ReleasesRoot), "foreign", "2026.10.08.20", "pb.exe"),
	} {
		if windowsServiceExecutableOwned(layout, path) {
			t.Fatalf("foreign executable accepted: %q", path)
		}
	}
}

func TestWindowsServiceDeclarationBindsExpectedExecutableBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb.exe")
	bytes := []byte("test-owned native executable")
	if err := os.WriteFile(path, bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(bytes)
	config := Config{Kind: HostdKind, Instance: "u0123456789abcdef01234567", Executable: path, ExecutableSHA256: hex.EncodeToString(digest[:]), ExecutableLength: int64(len(bytes))}
	body, err := renderWindowsService(config)
	if err != nil {
		t.Fatal(err)
	}
	var definition windowsServiceDefinition
	if err := json.Unmarshal(body, &definition); err != nil {
		t.Fatal(err)
	}
	if definition.ExecutableSHA256 != config.ExecutableSHA256 || definition.ExecutableLength != config.ExecutableLength {
		t.Fatal("native declaration did not bind approved bytes")
	}
	config.ExecutableSHA256 = strings.Repeat("a", 64)
	if _, err := renderWindowsService(config); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("changed bytes accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := renderWindowsService(config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing approved image accepted: %v", err)
	}
}
