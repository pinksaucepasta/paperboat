//go:build windows

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Review of 083679b72: the role lock is created SYSTEM/Administrators-only, and a lock file another user could
// have made (an inherited, unprotected DACL) is refused instead of locked.
func TestWindowsCarrierRoleLockCustody(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "runtime", "carrier-0.lock")
	unlock, err := lockCarrierFile(fresh)
	if err != nil {
		t.Fatalf("a fresh role lock was refused: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	// Reopening the file it created passes the same checks.
	unlock, err = lockCarrierFile(fresh)
	if err != nil {
		t.Fatalf("the role lock's own file was refused on reopen: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	squatted := filepath.Join(dir, "squatted.lock")
	if err := os.WriteFile(squatted, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockCarrierFile(squatted); !errors.Is(err, ErrProductionInvalid) {
		if unlock != nil {
			_ = unlock()
		}
		t.Fatalf("a role lock file with an inherited DACL was accepted: %v", err)
	}
}
