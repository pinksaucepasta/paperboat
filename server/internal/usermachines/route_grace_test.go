package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestWaitWhileNoRowsWaitsOutAHandoverAndNothingElse(t *testing.T) {
	calls := 0
	value, err := waitWhileNoRows(context.Background(), time.Second, 2*time.Millisecond, func() (int, error) {
		calls++
		if calls < 5 {
			return 0, sql.ErrNoRows
		}
		return 9, nil
	})
	if err != nil || value != 9 || calls != 5 {
		t.Fatalf("a route that came back was not found: %d %v %d", value, err, calls)
	}
	boom := errors.New("database down")
	calls = 0
	if _, err := waitWhileNoRows(context.Background(), time.Second, 2*time.Millisecond, func() (int, error) { calls++; return 0, boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("another error was retried: %v %d", err, calls)
	}
	started := time.Now()
	if _, err := waitWhileNoRows(context.Background(), 40*time.Millisecond, 2*time.Millisecond, func() (int, error) { return 0, sql.ErrNoRows }); !errors.Is(err, sql.ErrNoRows) || time.Since(started) > time.Second {
		t.Fatalf("a route that stays missing was not reported at the grace: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	started = time.Now()
	if _, err := waitWhileNoRows(cancelled, 5*time.Second, 50*time.Millisecond, func() (int, error) { return 0, sql.ErrNoRows }); !errors.Is(err, sql.ErrNoRows) || time.Since(started) > time.Second {
		t.Fatalf("a cancelled request kept waiting: %v", err)
	}
}
