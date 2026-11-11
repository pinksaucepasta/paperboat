package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// F21-159: the lock excludes a second holder (the kernel drops it when a process dies, which Close models),
// waits only as long as its context, and is free again after release.
func TestLockExcludesAndIsBoundedByTheContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.lock")
	release, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(short, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock was taken or the wait was unbounded: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("lock not free after release: %v", err)
	}
	if err := again(); err != nil {
		t.Fatal(err)
	}
}
