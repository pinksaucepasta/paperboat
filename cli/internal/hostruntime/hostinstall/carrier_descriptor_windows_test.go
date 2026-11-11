//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A descriptor path that is not a regular file is refused before anything is opened or read from it.
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
