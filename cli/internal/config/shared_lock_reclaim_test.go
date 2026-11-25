package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// Client wedge 2026-11-25: several waiters found the same dead owner; a late one removed the directory of the waiter
// that had already reclaimed it (errno 39 and 2 in its owner write), and an owner record of a live process that did
// not hold the lock stayed behind. local is nil: each waiter stands for its own process.
func TestSharedLockWaitersReclaimADeadOwnerOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secure-root")
	if err := createSharedLockDirectory(root); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	for round := range 40 {
		path := filepath.Join(root, "profile"+strconv.Itoa(round)+".json.lock.d")
		if err := createSharedLockDirectory(path); err != nil {
			t.Fatal(err)
		}
		owner := `{"pid":999999999,"hostname":` + strconv.Quote(hostname) + `,"created_at":"2026-07-11T00:00:00Z","token":"dead"}`
		if err := writeSharedLockOwner(filepath.Join(path, "owner.json"), []byte(owner)); err != nil {
			t.Fatal(err)
		}
		var holders atomic.Int32
		var wait sync.WaitGroup
		start := make(chan struct{})
		for range 12 {
			wait.Go(func() {
				lock := &sharedLock{path: path}
				<-start
				if err := lock.Lock(); err != nil {
					t.Errorf("round %d: %v", round, err)
					var errno syscall.Errno
					if errors.As(err, &errno) {
						t.Errorf("errno %d: %v", int(errno), errno)
					}
					return
				}
				if holders.Add(1) != 1 {
					t.Errorf("round %d: two holders", round)
				}
				holders.Add(-1)
				if err := lock.Unlock(); err != nil {
					t.Error(err)
				}
			})
		}
		close(start)
		wait.Wait()
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("round %d: lock left behind: %v", round, err)
		}
		if t.Failed() {
			return
		}
	}
}
