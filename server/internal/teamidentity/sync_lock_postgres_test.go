package teamidentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// blockingWorkOS holds the sync inside its first WorkOS read until the context ends.
type blockingWorkOS struct {
	fakeWorkOS
	entered chan struct{}
}

func (b *blockingWorkOS) Events(ctx context.Context, _ string, _ int) ([]Event, string, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, "", ctx.Err()
}

// A sync whose lock connection dies stops (the lock is gone on the server, so another process may run its own
// tick) and releases what it holds.
func TestSyncStopsWhenItsSessionLockConnectionDiesOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workos := &blockingWorkOS{entered: make(chan struct{}, 1)}
	s := New(store, workos, config.Default().Business, config.Default().Mail.InvitationLifetimeSeconds, "https://console.example.test/team", &fakeSeats{})
	done := make(chan error, 1)
	go func() { done <- s.Sync(ctx) }()
	select {
	case <-workos.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("sync never started its WorkOS read")
	}
	var terminated int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(pg_terminate_backend(l.pid)) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE l.locktype='advisory' AND l.objid=160236 AND l.granted AND a.datname=current_database()`).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("terminated %d lock holders, err=%v; want the one sync lock", terminated, err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errSyncLockLost) {
			t.Fatalf("sync returned %v, want errSyncLockLost", err)
		}
	case <-time.After(2*syncLockCheck + 15*time.Second):
		t.Fatal("sync kept running after its lock connection died")
	}
	var held int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=160236 AND granted`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("sync lock still held: %d, err=%v", held, err)
	}
}
