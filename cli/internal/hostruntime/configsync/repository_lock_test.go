package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F21-159: a branch lock left by a killed publisher is cleared once it is older than any live publish could
// take, while a younger lock (another writer) still blocks and is never altered.
func TestStaleRepositoryLockIsClearedAndALiveOneIsNot(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "master.lock")
	if err := os.WriteFile(lockPath, []byte("other writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openRepositoryLock(lockPath, 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("a fresh lock was taken: %v", err)
	}
	if data, _ := os.ReadFile(lockPath); string(data) != "other writer" {
		t.Fatal("a live writer's lock was altered")
	}
	old := time.Now().Add(-2 * staleRepositoryLock)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	lock, err := openRepositoryLock(lockPath, 0o644)
	if err != nil {
		t.Fatalf("a stale lock was not cleared: %v", err)
	}
	defer lock.Close()
	if info, err := os.Stat(lockPath); err != nil || info.Size() != 0 {
		t.Fatalf("the retaken lock is not a new empty file: %v %v", info, err)
	}
}
