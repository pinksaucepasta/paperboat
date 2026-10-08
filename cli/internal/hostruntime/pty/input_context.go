//go:build darwin || linux || windows

package pty

import (
	"context"
	"os"
	"sync"
	"time"
)

// LockInput makes queued input honor cancellation instead of holding an owner
// admission after the feature connection has gone away.
func LockInput(ctx context.Context, mutex *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mutex.TryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if mutex.TryLock() {
			return nil
		}
	}
}

func inputTimeout(ctx context.Context) time.Duration {
	timeout := 20 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	return timeout
}

func WriteInputContext(ctx context.Context, file *os.File, data []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return writeInput(file, data, inputTimeout(ctx))
}
