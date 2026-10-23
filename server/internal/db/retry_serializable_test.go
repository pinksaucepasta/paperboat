package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A transaction managed with BeginSerializable is replayed after a serialization failure or deadlock,
// so one conflict (two servers overlapping in a rollout) is not a 503; any other error is returned at once,
// and the replays are bounded.
func TestRetrySerializableReplaysOnlyConflictsAndIsBounded(t *testing.T) {
	ctx := context.Background()
	conflict := fmt.Errorf("resolve native private target: %w", &pgconn.PgError{Code: "40001"})
	calls := 0
	if err := RetrySerializable(ctx, func() error {
		calls++
		if calls < 3 {
			return conflict
		}
		return nil
	}); err != nil || calls != 3 {
		t.Fatalf("conflicts then success: err=%v calls=%d", err, calls)
	}
	calls = 0
	deadlock := &pgconn.PgError{Code: "40P01"}
	if err := RetrySerializable(ctx, func() error { calls++; return deadlock }); !errors.Is(err, deadlock) || calls != serializableAttempts {
		t.Fatalf("persistent deadlock: err=%v calls=%d, want %d bounded attempts", err, calls, serializableAttempts)
	}
	calls = 0
	other := errors.New("not found")
	if err := RetrySerializable(ctx, func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("other error: err=%v calls=%d, want one call", err, calls)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	calls = 0
	if err := RetrySerializable(canceled, func() error { calls++; return conflict }); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled: err=%v calls=%d", err, calls)
	}
}
