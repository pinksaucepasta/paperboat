package hostinstall

import (
	"context"
	"time"
)

const (
	completeMaintenanceBudget  = 30 * time.Second
	completeMaintenanceAttempt = 5 * time.Second
	completeMaintenanceGap     = time.Second
)

// completeMaintenanceRetrying releases hostd's startup fence after the durable commit. One 5 s call failed on a busy
// Windows runner although the machine was committed and online (pair printed "committing runtime state"); the Unix
// hostd retries every second until it sees the commit (maintenance_commit_unix.go), so this does too, bounded.
// The last error is returned when the budget ends, so a commit that really cannot complete still fails.
func completeMaintenanceRetrying(budget, gap time.Duration, attempt func(context.Context) error) error {
	deadline := time.Now().Add(budget)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), completeMaintenanceAttempt)
		err := attempt(ctx)
		cancel()
		if err == nil || !time.Now().Add(gap).Before(deadline) {
			return err
		}
		time.Sleep(gap)
	}
}
