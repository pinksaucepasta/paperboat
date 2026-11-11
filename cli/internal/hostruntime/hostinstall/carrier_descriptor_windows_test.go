//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A descriptor path that is not a regular file is refused on the handle that was opened, and nothing is decoded from it.
func TestReadWindowsCarrierFileRefusesAnIrregularPath(t *testing.T) {
	instance, err := WindowsInstanceForSID("S-1-5-21-1-2-3-1001")
	if err != nil || carrierConfigPath(instance) == "" {
		t.Fatalf("no carrier descriptor path for a test instance: %q %v", instance, err)
	}
	directory := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readWindowsCarrierFile(context.Background(), instance, directory); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a directory at the descriptor path was read: %v", err)
	}
}

// A link at the descriptor path is not followed: the handle is the link itself, which is refused (audit C #34).
func TestReadWindowsCarrierFileDoesNotFollowALink(t *testing.T) {
	instance, err := WindowsInstanceForSID("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("cannot create a symbolic link here: ", err)
	}
	if _, err := readWindowsCarrierFile(context.Background(), instance, link); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a link at the descriptor path was followed: %v", err)
	}
}
