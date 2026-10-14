package enrollment

import (
	"context"
	"errors"
	"sync"
	"time"
)

const identityRenewalLockPollInterval = 25 * time.Millisecond

// lockRuntimeIdentityRenewal serializes renewal across daemon and worker
// processes sharing an account's identity store. The lock file is persistent;
// deleting it after unlock could let concurrent callers lock different inodes.
func lockRuntimeIdentityRenewal(ctx context.Context, stateRoot string) (func() error, error) {
	lock, err := openRuntimeIdentityRenewalLock(stateRoot)
	if err != nil {
		return nil, err
	}
	closeOnError := func(cause error) (func() error, error) {
		return nil, errors.Join(cause, lock.Close())
	}
	for {
		if err := ctx.Err(); err != nil {
			return closeOnError(err)
		}
		acquired, err := lock.Try()
		if err != nil {
			return closeOnError(err)
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				return closeOnError(err)
			}
			var once sync.Once
			var closeErr error
			return func() error {
				once.Do(func() { closeErr = lock.Close() })
				return closeErr
			}, nil
		}
		timer := time.NewTimer(identityRenewalLockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return closeOnError(ctx.Err())
		case <-timer.C:
		}
	}
}
