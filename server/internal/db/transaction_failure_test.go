package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
)

func TestIsSerializationFailureRetriesOnlyPurePostgresLeaves(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			cause := &pgconn.PgError{Code: code}
			if !isSerializationFailure(cause) || !isSerializationFailure(fmt.Errorf("wrapped: %w", cause)) {
				t.Fatalf("pure PostgreSQL failure %s was not recognized", code)
			}
		})
	}

	serializable := &pgconn.PgError{Code: "40001"}
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "joined operational failure", err: errors.Join(serializable, syscall.EIO)},
		{name: "cleanup failure", err: &transactionCleanupFailure{operation: serializable, rollback: syscall.EIO}},
		{name: "cyclic wrapper", err: cyclicTransactionErrorValue()},
		{name: "typed nil postgres error", err: typedNilPostgresError()},
		{name: "uncomparable leaf", err: uncomparableTransactionError{parts: []string{"unknown"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if isSerializationFailure(test.err) {
				t.Fatal("non-proven transaction error was classified as retryable")
			}
		})
	}

	// Comparability is not required for a complete unary wrapper chain; the
	// classifier does not use error values as map keys or call Error().
	uncomparable := uncomparableTransactionWrapper{cause: serializable, parts: []string{"wrapper"}}
	if !isSerializationFailure(uncomparable) {
		t.Fatal("uncomparable unary wrapper around a PostgreSQL error was rejected")
	}
}

func TestInTxPreservesCallbackCauseAfterConnectionLoss(t *testing.T) {
	store := openTransactionFailureTestDB(t)
	cause := errors.New("private callback cause")
	callbackErr := panicFormattingTransactionError{cause: cause}
	calls := 0
	var connectionCloseErr error
	err := store.InTx(context.Background(), func(ctx context.Context, tx *Tx) error {
		calls++
		connectionCloseErr = tx.tx.Conn().Close(ctx)
		return callbackErr
	})
	if connectionCloseErr != nil {
		t.Fatalf("close transaction connection failed: %T", connectionCloseErr)
	}

	var cleanup *transactionCleanupFailure
	if !errors.As(err, &cleanup) || cleanup.rollback == nil || !errors.Is(err, cause) {
		t.Fatalf("transaction cleanup did not preserve both failures: cleanup=%v original=%t", cleanup != nil, errors.Is(err, cause))
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want one", calls)
	}
	stats := store.TransactionStats().Serializable
	if stats.Calls != 1 || stats.Attempts != 1 || stats.Retries != 0 {
		t.Fatalf("cleanup failure transaction stats = %+v", stats)
	}

	queryCtx, cancelQuery := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelQuery()
	var one int
	if queryErr := store.Pool().QueryRow(queryCtx, "SELECT 1").Scan(&one); queryErr != nil || one != 1 {
		t.Fatalf("fresh query after closed-connection rollback: value=%d error_type=%T", one, queryErr)
	}
}

func TestInTxDoesNotReplayCleanupFailureWithSerializationCode(t *testing.T) {
	store := openTransactionFailureTestDB(t)
	calls := 0
	var connectionCloseErr error
	err := store.InTx(context.Background(), func(ctx context.Context, tx *Tx) error {
		calls++
		connectionCloseErr = tx.tx.Conn().Close(ctx)
		return &pgconn.PgError{Code: "40001"}
	})
	if connectionCloseErr != nil {
		t.Fatalf("close transaction connection failed: %T", connectionCloseErr)
	}

	var cleanup *transactionCleanupFailure
	var postgres *pgconn.PgError
	if !errors.As(err, &cleanup) || cleanup.rollback == nil || !errors.As(err, &postgres) || postgres.Code != "40001" {
		t.Fatalf("cleanup wrapper did not retain serialization callback cause: cleanup=%t postgres=%t", cleanup != nil, postgres != nil)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times after cleanup failure, want one", calls)
	}
	stats := store.TransactionStats().Serializable
	if stats.Calls != 1 || stats.Attempts != 1 || stats.Retries != 0 {
		t.Fatalf("cleanup failure transaction stats = %+v", stats)
	}
}

func openTransactionFailureTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the transaction cleanup PostgreSQL test")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open isolated transaction test database: %T", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close isolated transaction test database: %T", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("ping isolated transaction test database: %T", err)
	}
	return store
}

type panicFormattingTransactionError struct{ cause error }

func (panicFormattingTransactionError) Error() string   { panic("transaction error was formatted") }
func (e panicFormattingTransactionError) Unwrap() error { return e.cause }

type cycleTransactionError struct{ next error }

func (cycleTransactionError) Error() string   { return "cycle" }
func (e cycleTransactionError) Unwrap() error { return e.next }

func cyclicTransactionErrorValue() error {
	e := &cycleTransactionError{}
	e.next = e
	return e
}

type uncomparableTransactionError struct{ parts []string }

func (uncomparableTransactionError) Error() string { return "uncomparable" }

type uncomparableTransactionWrapper struct {
	cause error
	parts []string
}

func (uncomparableTransactionWrapper) Error() string   { return "uncomparable wrapper" }
func (e uncomparableTransactionWrapper) Unwrap() error { return e.cause }

func typedNilPostgresError() error {
	var pgErr *pgconn.PgError
	return pgErr
}
