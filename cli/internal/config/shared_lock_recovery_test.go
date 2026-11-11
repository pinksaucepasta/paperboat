package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedLockRecognizesAbandonedOwnerMetadata(t *testing.T) {
	hostname, _ := os.Hostname()
	for _, metadata := range []string{""} {
		t.Run(metadata, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lock.d")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if metadata != "" {
				if err := os.WriteFile(filepath.Join(path, "owner.json"), []byte(metadata), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if stale, err := sharedLockIsStale(path, hostname); err != nil || stale {
				t.Fatalf("fresh incomplete owner: stale=%v err=%v", stale, err)
			}
			old := time.Now().Add(-time.Minute)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			if stale, err := sharedLockIsStale(path, hostname); err != nil || !stale {
				t.Fatalf("abandoned owner: stale=%v err=%v", stale, err)
			}
		})
	}
}

func TestSharedLockRecoversAbandonedDirectoryAndPreservesLiveOwner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secure-root")
	if err := createSharedLockDirectory(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "profiles", "profile.lock")
	lock := newSharedLock(path)
	if err := prepareSharedLockParent(filepath.Dir(lock.path)); err != nil {
		t.Fatal(err)
	}
	if err := createSharedLockDirectory(lock.path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock.path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	// Age alone cannot reclaim a valid lock held by this still-running process. (The owner record's own
	// CreatedAt cannot be older than the process: that is a reused PID, see TestSharedLockIsNotTrustedAcrossAReusedPID.)
	if err := os.Chtimes(lock.path, old.Add(-time.Hour), old.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	if stale, err := sharedLockIsStale(lock.path, hostname); err != nil || stale {
		t.Fatalf("live owner reclaimed: stale=%v err=%v", stale, err)
	}
}

func TestSharedLockDoesNotTreatOwnerReadFailureAsAbandonment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.d")
	if err := os.MkdirAll(filepath.Join(path, "owner.json"), 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if stale, err := sharedLockIsStale(path, "local"); err == nil || stale {
		t.Fatalf("read failure accepted: stale=%v err=%v", stale, err)
	}
}

func TestEmptyLockCleanupPreservesPublishedOwner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secure-root")
	if err := createSharedLockDirectory(root); err != nil {
		t.Fatal(err)
	}
	lock := newSharedLock(filepath.Join(root, "profiles", "profile.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	if err := cleanupNewSharedLock(lock.path); err == nil {
		t.Fatal("removed newly published owner")
	}
	if _, err := os.Stat(filepath.Join(lock.path, "owner.json")); err != nil {
		t.Fatal(err)
	}
}
