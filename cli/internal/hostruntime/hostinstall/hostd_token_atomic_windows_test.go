//go:build windows

package hostinstall

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// F21-159 on Windows: the hostd token is written by atomicfile (temp file + replace), so it is complete the moment it
// exists (32 bytes), no temp file stays beside it, an existing token is kept as it is, and a token path that is not a
// regular file is refused without writing (the atomicfile tests cover a replacement that fails half way).
func TestWindowsHostdTokenIsCompleteAndAtomic(t *testing.T) {
	if !isAdministrator() {
		t.Skip("requires an elevated Windows token: the token's SYSTEM owner needs the restore privilege")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := user.User.Sid.String()
	dir := t.TempDir()
	path := filepath.Join(dir, "hostd.token")
	if err := ensureWindowsTokenAt(path, ownerSID); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil || len(first) != 32 {
		t.Fatalf("token = %d bytes, %v; want 32 bytes", len(first), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temp files left beside the token: %v", entries)
	}
	if err := ensureWindowsTokenAt(path, ownerSID); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(first, again) {
		t.Fatal("an existing token was replaced")
	}
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsTokenAt(blocked, ownerSID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a directory at the token path: err=%v, want ErrInvalidRequest", err)
	}
	if entries, _ := os.ReadDir(blocked); len(entries) != 0 {
		t.Fatalf("a refused token path was written into: %v", entries)
	}
}
