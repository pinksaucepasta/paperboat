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
	"github.com/pinksaucepasta/paperboat/server/internal/config"
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

// A callback that fails on a connection that is gone keeps its own error: the server ended the transaction with the
// session and nothing was committed, so there is no cleanup to report (it used to surface as a connLockError that hid
// the cause: 101 edge 503s, 2026-10-23..26).
func TestInTxKeepsCallbackErrorAfterConnectionLoss(t *testing.T) {
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
	if errors.As(err, &cleanup) || !errors.Is(err, cause) {
		t.Fatalf("closed-connection rollback: cleanup failure=%t original=%t, want the callback's error alone", cleanup != nil, errors.Is(err, cause))
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want one", calls)
	}
	queryCtx, cancelQuery := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelQuery()
	var one int
	if queryErr := store.Pool().QueryRow(queryCtx, "SELECT 1").Scan(&one); queryErr != nil || one != 1 {
		t.Fatalf("fresh query after closed-connection rollback: value=%d error_type=%T", one, queryErr)
	}
}

// Nothing was committed when the connection is gone, so a serialization failure is retried like any other.
func TestInTxRetriesSerializationFailureOnAClosedConnection(t *testing.T) {
	store := openTransactionFailureTestDB(t)
	calls := 0
	err := store.InTx(context.Background(), func(ctx context.Context, tx *Tx) error {
		calls++
		if calls > 1 {
			return nil
		}
		if err := tx.tx.Conn().Close(ctx); err != nil {
			t.Fatalf("close transaction connection failed: %T", err)
		}
		return &pgconn.PgError{Code: "40001"}
	})
	if err != nil || calls != 2 {
		t.Fatalf("retry after closed connection: err=%v calls=%d, want nil and 2", err, calls)
	}
}

// A request that is canceled while its connection is healthy still rolls back (on a context of its own): the
// callback's error is reported alone and the connection returns to the pool.
func TestInTxRollsBackAfterTheRequestContextIsCanceled(t *testing.T) {
	store := openTransactionFailureTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := store.Pool().Stat().NewConnsCount()
	err := store.InTx(ctx, func(ctx context.Context, tx *Tx) error {
		cancel()
		return ctx.Err()
	})
	var cleanup *transactionCleanupFailure
	if errors.As(err, &cleanup) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: cleanup failure=%t canceled=%t, want context.Canceled alone", cleanup != nil, errors.Is(err, context.Canceled))
	}
	var one int
	if err := store.Pool().QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query after canceled request: %v", err)
	}
	if opened := store.Pool().Stat().NewConnsCount() - before; opened > 0 {
		t.Fatalf("a canceled request destroyed its pooled connection: %d new connections", opened)
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
