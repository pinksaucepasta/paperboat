package localwait

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// InventoryRefreshEvery and InventoryRefreshMaximum pace the refreshes a waiting command asks its own daemon
// for: the first after InventoryRefreshEvery, then doubling to InventoryRefreshMaximum. A machine's first
// sighting and its changes reach the daemon as inventory events, so a long wait needs no fast polling: at a
// fixed 3 s each waiting command cost the server a full inventory (one list per workspace) every 3 s.
const (
	InventoryRefreshEvery   = 3 * time.Second
	InventoryRefreshMaximum = 15 * time.Second
)

// KeepInventoryFresh calls refresh (at most one call in flight) after every, then after doubling waits up to
// maximum, until ctx ends. The wait reports what it last saw when its own deadline passes; a refresh failure is
// recorded in the diagnostic log (each distinct failure once) so a wait that timed out can be explained.
func KeepInventoryFresh(ctx context.Context, refresh func(context.Context) error, every, maximum time.Duration) {
	if refresh == nil || every <= 0 || maximum < every {
		return
	}
	timer := time.NewTimer(every)
	defer timer.Stop()
	var lastFailure string
	for wait := every; ; {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := refresh(ctx); err != nil && ctx.Err() == nil && err.Error() != lastFailure {
				lastFailure = err.Error()
				diagnosticlog.TryWarn("inventory refresh failed while waiting", "error", lastFailure)
			}
			wait = min(wait*2, maximum)
			timer.Reset(wait)
		}
	}
}
