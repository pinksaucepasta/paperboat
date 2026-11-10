package machineguard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// An install waits guardAttemptTimeout for the machine guard's pipe after the scheduled task was run and, when it
// has not come, stops and runs the task again, up to guardStartAttempts times. On a machine whose cores are all busy
// the first start of the guard can get no CPU at all (winpool-6, one spinner per core: 5 of 5 fresh installs, the
// process existed for 56 s at 0 CPU and the pipe never appeared, then 'open \\.\pipe\PaperboatMachineGuard: The
// system cannot find the file specified' and a rollback that failed on the guard's image), while a restart under the same
// load answered in 2 s. An unloaded install needs about 13 s before the guard even starts, hence 20 s per attempt.
var (
	guardAttemptTimeout = 20 * time.Second
	guardStartAttempts  = 3
)

// startGuardWithRetry waits for the guard and restarts it between attempts; the last failure is returned.
func startGuardWithRetry(ctx context.Context, probe func(context.Context) error, restart func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := waitGuardReady(ctx, guardAttemptTimeout, probe)
		if err == nil || attempt >= guardStartAttempts || ctx.Err() != nil {
			return err
		}
		if restartErr := restart(ctx); restartErr != nil {
			return errors.Join(err, fmt.Errorf("restart the machine guard task: %w", restartErr))
		}
	}
}

// removeGuardImage removes the guard's pb.exe during a rollback. The task was just ended and Windows keeps the image
// locked (access denied / sharing violation) until the process is gone, so a refusal is retried for a bounded time.
var removeGuardFile = os.Remove

func removeGuardImage(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := removeGuardFile(path)
		if err == nil || errors.Is(err, os.ErrNotExist) || !lockedGuardImage(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func lockedGuardImage(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, fs.ErrPermission) || errors.As(err, &errno) && errno == 32 // ERROR_SHARING_VIOLATION
}

// waitGuardReady calls probe (500 ms each) until it succeeds, timeout passes or ctx ends.
func waitGuardReady(ctx context.Context, timeout time.Duration, probe func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := probe(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("machine guard did not become ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
