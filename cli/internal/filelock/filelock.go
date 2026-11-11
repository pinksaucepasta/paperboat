// Package filelock is an advisory cross-process lock on one file, released by the kernel when the holder dies,
// so a crash never leaves a stale lock (unlike a lock file that has to be removed).
package filelock

import (
	"context"
	"sync"
	"time"
)

const pollInterval = 25 * time.Millisecond

// Lock takes the exclusive lock on path (creating it 0600) and returns the release function. It waits until ctx
// ends: a held lock is never an unbounded wait.
func Lock(ctx context.Context, path string) (func() error, error) {
	file, err := open(path)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		busy, err := tryLock(file)
		if err == nil && !busy {
			var once sync.Once
			var releaseErr error
			// Once: a second release on a closed descriptor number another open may now own would unlock that file.
			return func() error { once.Do(func() { releaseErr = unlock(file) }); return releaseErr }, nil
		}
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		timer.Reset(pollInterval)
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
