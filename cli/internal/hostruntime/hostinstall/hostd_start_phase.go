//go:build darwin || linux || windows

package hostinstall

import (
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// hostdStartPhase says how far the host service's start got when its health check gave up, from what the service
// answers on its lifecycle pipe, so the failure names what the service was waiting on (Windows Sandbox, 2026-11-10:
// two updates rolled back on this check and the log held only the probe's timeout). A worker sends its first
// heartbeat once its runtime has started, and that runtime serves the health endpoint.
func hostdStartPhase(status hostdproto.Status, err error, now time.Time) error {
	switch {
	case err != nil:
		return fmt.Errorf("host service start phase: its owner process does not answer on its pipe (not started, or not past its own start): %w", err)
	case status.State == hostdproto.StateEmpty:
		return errors.New("host service start phase: the owner process answers, but no runtime worker has registered with it")
	case status.State == hostdproto.StateCandidate:
		return fmt.Errorf("host service start phase: runtime worker %s registered but was not activated", status.WorkerID)
	case status.LastHeartbeatUnixMilli == 0:
		return fmt.Errorf("host service start phase: runtime worker %s is activated but its runtime has not finished starting (no heartbeat yet)", status.WorkerID)
	default:
		age := now.Sub(time.UnixMilli(status.LastHeartbeatUnixMilli)).Round(time.Second)
		return fmt.Errorf("host service start phase: runtime worker %s started (last heartbeat %s ago), but its health endpoint does not answer", status.WorkerID, age)
	}
}
