package machineevents

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// eventually polls ok until it holds (10 s bound) so a test proceeds the moment its precondition is true,
// never after a fixed sleep.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitListening returns once a session has issued LISTEN on the events channel.
func waitListening(t *testing.T, ctx context.Context, database *db.DB) {
	t.Helper()
	eventually(t, "the LISTEN session", func() bool {
		var n int
		err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND query='LISTEN `+channel+`'`).Scan(&n)
		return err == nil && n > 0
	})
}

// waitSubscribed returns once every key has a waiter registered on the hub.
func waitSubscribed(t *testing.T, h *Hub, keys ...string) {
	t.Helper()
	eventually(t, "the waiter to subscribe", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, key := range keys {
			if len(h.subs[key]) == 0 {
				return false
			}
		}
		return true
	})
}
