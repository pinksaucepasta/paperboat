package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// F21-157: a serializable transaction that manages its own database/sql transaction replays after 40001
// only inside RetrySerializable, so no production file may call BeginSerializable more often than it calls RetrySerializable.
func TestEveryBeginSerializableCallerRetries(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "db.go" {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(source)
		if strings.Count(text, "BeginSerializable(") > strings.Count(text, "RetrySerializable(") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("BeginSerializable without RetrySerializable in: %v", offenders)
	}
}
