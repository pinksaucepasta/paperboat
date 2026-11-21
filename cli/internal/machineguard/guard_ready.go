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
// load answered in 2 s once its image was warm. The cause (spark/guard-blocked-diag.md): the BelowNormal cold loader
// crawls through page faults of the 99 MB image, and a 20 s attempt with an /End killed each start before it finished.
// So the task runs at Normal priority, the image is read once before it is registered (warmGuardImage), and one
// attempt gets a minute; a second attempt follows only when the first still shows no pipe.
var (
	guardAttemptTimeout = time.Minute
	guardStartAttempts  = 2
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
		//paperboat:allow-source-policy sleep owner=machineguard reason=bounded-locked-image-remove-retry
		time.Sleep(100 * time.Millisecond)
	}
}

func lockedGuardImage(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, fs.ErrPermission) || errors.As(err, &errno) && errno == 32 // ERROR_SHARING_VIOLATION
}

// macpool-6 at load 13-21: a 10 s wait with 500 ms probes timed out and rolled back a rev12->rev15 update that
// passes idle; the Windows start attempt already allows a minute.
const (
	guardInstallReadyTimeout = time.Minute
	guardProbeTimeout        = 2 * time.Second
	guardReadyStability      = 300 * time.Millisecond
)

func probeGuardStatus(ctx context.Context, socket string, revision int) error {
	client, err := Connect(ctx, socket)
	if err != nil {
		return fmt.Errorf("connect to the machine guard: %w", err)
	}
	defer client.Close()
	status, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("read machine guard status: %w", err)
	}
	return guardRunsRevision(status, revision)
}

// guardRunsRevision refuses a reply from another guard revision: the replaced guard can still be answering
// on the socket while the service manager swaps it, and must not pass the new guard's readiness.
func guardRunsRevision(status RuntimeStatus, want int) error {
	if want != 0 && status.Revision != want {
		return fmt.Errorf("machine guard revision %d is still answering; waiting for revision %d", status.Revision, want)
	}
	return nil
}

// waitGuardReady requires repeated ready replies through the stability window.
// A dropped connection or a non-ready reply restarts that window.
func waitGuardReady(ctx context.Context, timeout time.Duration, probe func(context.Context) error) error {
	wait, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	var readySince time.Time
	var lastErr error
	for wait.Err() == nil {
		probeCtx, cancel := context.WithTimeout(wait, guardProbeTimeout)
		err := probe(probeCtx)
		cancel()
		if err != nil {
			lastErr = err
			readySince = time.Time{}
		} else if readySince.IsZero() {
			readySince = time.Now()
		} else if time.Since(readySince) >= guardReadyStability && wait.Err() == nil {
			return nil
		}
		select {
		case <-wait.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lastErr == nil {
		lastErr = wait.Err()
	}
	return fmt.Errorf("machine guard did not become ready: %w", lastErr)
}
