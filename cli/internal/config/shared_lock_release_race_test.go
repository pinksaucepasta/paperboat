package config

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// A waiter that finds another process's lock directory inspects it; that process may release it at that moment. On
// Windows the inspection then fails (the directory is gone before the open, or under the open handle). That is "not
// acquired yet": victus 2026-11-10 failed credential reads on it under ten parallel pb.exe.
func TestSharedLockWaiterSurvivesReleaseDuringInspection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json.lock")
	lock := newSharedLock(path)
	// Another live process holds the lock: its directory with its owner record.
	if err := createSharedLockDirectory(lock.path); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	owner := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"hostname":` + strconv.Quote(hostname) + `,"created_at":"2026-11-10T00:00:00Z","token":"other"}`
	if err := writeSharedLockOwner(filepath.Join(lock.path, "owner.json"), []byte(owner)); err != nil {
		t.Fatal(err)
	}
	inspections := 0
	inspectSharedLock = func(directory string) error {
		inspections++
		// The holder releases while the waiter inspects: the directory is gone when the waiter opens it.
		if err := removeSharedLock(directory); err != nil {
			t.Error(err)
		}
		return syscall.ENOENT
	}
	t.Cleanup(func() { inspectSharedLock = validateSharedLockDirectory })
	if err := lock.Lock(); err != nil {
		t.Fatalf("waiter failed on a lock released during its inspection: %v", err)
	}
	if inspections != 1 {
		t.Fatalf("inspections = %d, want 1", inspections)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

// A lock directory that stays invalid (a foreign owner or ACL) is still refused, after the grace.
func TestSharedLockRefusesDirectoryThatStaysInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json.lock")
	lock := newSharedLock(path)
	if err := createSharedLockDirectory(lock.path); err != nil {
		t.Fatal(err)
	}
	inspectSharedLock = func(string) error { return ErrCredentialStoreUnavailable }
	t.Cleanup(func() { inspectSharedLock = validateSharedLockDirectory })
	if err := lock.Lock(); err == nil || err.Error() != "shared credential lock could not be validated" {
		t.Fatalf("invalid lock directory = %v", err)
	}
}
