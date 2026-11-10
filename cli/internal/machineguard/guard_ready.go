package machineguard

import (
	"context"
	"fmt"
	"time"
)

// guardReadyTimeout bounds how long an install waits for the machine guard's pipe after the scheduled task was run.
// Task Scheduler starts the process when it gets to it: on a machine whose cores are all busy that took over the
// 10 s this once was (winpool-6, one spinner per core: 4 of 4 installs failed with 'open \\.\pipe\PaperboatMachineGuard:
// The system cannot find the file specified' and the rollback then failed on the guard's image).
var guardReadyTimeout = time.Minute

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
