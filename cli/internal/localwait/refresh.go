package localwait

import (
	"context"
	"time"
)

// InventoryRefreshEvery paces the refreshes a waiting command asks its own daemon for. The daemon no longer
// re-lists the account on every presence change (that woke every daemon of the account at once), so a command
// that waits for a machine to come up asks for the current inventory itself, at most this often.
const InventoryRefreshEvery = 3 * time.Second

// KeepInventoryFresh calls refresh once per every (at most one call in flight) until ctx ends. Failures are
// left to the wait itself: it reports what it last saw when its own deadline passes.
func KeepInventoryFresh(ctx context.Context, refresh func(context.Context) error, every time.Duration) {
	if refresh == nil || every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = refresh(ctx)
		}
	}
}
