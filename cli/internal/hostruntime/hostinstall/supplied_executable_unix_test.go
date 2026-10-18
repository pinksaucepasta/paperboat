//go:build darwin || linux

package hostinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Device finding (Debian, umask 0002): a copy in /tmp with mode 0775 failed with
// a bare "installation failed" while the same bytes at mode 0700 installed.
func TestCheckSuppliedExecutableNamesGroupWritableMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	if err := CheckSuppliedExecutable(path, uid); err != nil {
		t.Fatalf("mode 0755 refused: %v", err)
	}
	if err := os.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}
	err := CheckSuppliedExecutable(path, uid)
	var refused *SuppliedExecutableError
	if !errors.As(err, &refused) || !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "chmod go-w "+path) || !strings.Contains(err.Error(), "0775") {
		t.Fatalf("group-writable binary: %v", err)
	}
	if err := verifyArtifact(path, uid, "linux"); !errors.As(err, &refused) {
		t.Fatalf("privileged check does not report the same reason: %v", err)
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckSuppliedExecutable(link, uid); !errors.As(err, &refused) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink: %v", err)
	}
}
