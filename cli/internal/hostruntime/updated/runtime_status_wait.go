package updated

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// The new hostd's first heartbeat lands after its own health checks, so a single status read right after a
// cutover rolled a slow healthy start back (slot 6 .5 to .6, 2026-11-10). Waits read it until it is ready.
const (
	windowsFeatureReadyTimeout  = 90 * time.Second
	windowsTerminalReadyTimeout = 30 * time.Second
	windowsFeatureReadyInterval = 500 * time.Millisecond
)

func validWindowsRuntimeStatus(status hostdproto.Status, version string) bool {
	return status.State == hostdproto.StateActive && status.WorkerID == "runtime-"+strings.ReplaceAll(version, " ", "-") && status.Epoch > 0 && status.APIVersion > 0 && status.LastHeartbeatUnixMilli > 0 && time.Since(time.UnixMilli(status.LastHeartbeatUnixMilli)) <= 15*time.Second
}

// waitWindowsRuntimeStatus polls the runtime's status until it is the given version, active and inside the API
// range (apiMax 0 means no range). The error that ends the wait names what was last observed and joins the
// last read error and any context error, so a failure says why.
func waitWindowsRuntimeStatus(ctx context.Context, read func(context.Context) (hostdproto.Status, error), version string, apiMin, apiMax uint16, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	var lastErr error
	for {
		status, err := read(ctx)
		switch {
		case err != nil:
			lastErr, last = err, "status read failed"
		case !validWindowsRuntimeStatus(status, version) || (apiMax > 0 && (status.APIVersion < apiMin || status.APIVersion > apiMax)):
			lastErr, last = nil, fmt.Sprintf("state=%s worker=%s epoch=%d api=%d heartbeat_age=%s", status.State, status.WorkerID, status.Epoch, status.APIVersion, heartbeatAge(status))
		default:
			return nil
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(fmt.Errorf("%w: stopped while waiting for the new runtime (last seen: %s)", errInvalidWindowsActivation, last), lastErr, err)
		}
		if time.Now().Add(interval).After(deadline) {
			return errors.Join(fmt.Errorf("%w: the new runtime was not ready within %s (last seen: %s)", errInvalidWindowsActivation, timeout, last), lastErr)
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

func heartbeatAge(status hostdproto.Status) string {
	if status.LastHeartbeatUnixMilli == 0 {
		return "none"
	}
	return time.Since(time.UnixMilli(status.LastHeartbeatUnixMilli)).Round(time.Millisecond).String()
}
