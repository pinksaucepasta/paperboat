//go:build windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The role lock is this account's file with a protected DACL (the carrier runs as the enrolled owner's workload; a
// non-administrator could not create an Administrators-owned file). A lock file an older build left with an
// inherited DACL is replaced, not refused forever (winpool-13: the new carrier spun silently until its deadline and
// the rollback's carrier could not start). A lock another carrier holds is busy, and only busy is waited for.
func TestWindowsCarrierRoleLockCustody(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "runtime", "carrier-0.lock")
	unlock, err := lockCarrierFile(fresh)
	if err != nil {
		t.Fatalf("a fresh role lock was refused: %v", err)
	}
	if _, err := lockCarrierFile(fresh); !carrierLockBusy(err) {
		t.Fatalf("a held role lock was not reported busy: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if unlock, err = lockCarrierFile(fresh); err != nil {
		t.Fatalf("the role lock's own file was refused on reopen: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.lock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if unlock, err = lockCarrierFile(stale); err != nil {
		t.Fatalf("a role lock file with an inherited DACL was not replaced: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := awaitRoleLock(ctx, dir); err == nil || carrierLockBusy(err) || time.Since(started) > 2*time.Second {
		t.Fatalf("a role lock that cannot be a file was waited for (%v after %s)", err, time.Since(started))
	}
}
