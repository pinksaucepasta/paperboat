package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A transaction under a session lock holds the lock while it runs and releases it however it ends (commit,
// cancellation, panic), so the next waiter takes it at once; a waiter whose context ends stops waiting.
func TestInTxUnderSessionLockReleasesOnEveryExitOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	key, seed := fmt.Sprintf("session-lock-test/%d", time.Now().UnixNano()), int64(7311)
	free := func() bool {
		t.Helper()
		conn, err := store.Direct().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,$2))`, key, seed).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if locked {
			if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,$2))`, key, seed); err != nil {
				t.Fatal(err)
			}
		}
		return locked
	}

	// Held while fn runs; a cancelled caller releases it.
	running, cancelled := make(chan struct{}), make(chan error, 1)
	callerCtx, cancelCaller := context.WithCancel(ctx)
	go func() {
		cancelled <- store.InTxUnderSessionLock(callerCtx, key, seed, func(ctx context.Context, tx *Tx) error {
			close(running)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-running
	if free() {
		t.Fatal("the lock is not held while the transaction runs")
	}
	// A waiter stops at its own deadline.
	waitCtx, cancelWait := context.WithTimeout(ctx, 300*time.Millisecond)
	if err := store.InTxUnderSessionLock(waitCtx, key, seed, func(context.Context, *Tx) error { return nil }); err == nil {
		t.Fatal("a second transaction ran while the lock was held")
	}
	cancelWait()
	cancelCaller()
	if err := <-cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled transaction returned %v", err)
	}
	if !free() {
		t.Fatal("a cancelled transaction left its session lock held")
	}

	// A panic in fn closes the connection, which releases the lock.
	func() {
		defer func() { _ = recover() }()
		_ = store.InTxUnderSessionLock(ctx, key, seed, func(context.Context, *Tx) error { panic("boom") })
	}()
	// The server drops the session when it notices the closed connection, a moment after the panic returns.
	for deadline := time.Now().Add(5 * time.Second); !free(); {
		if time.Now().After(deadline) {
			t.Fatal("a panicking transaction left its session lock held")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A committed transaction releases it, and the same session may take it again as a transaction lock.
	if err := store.InTxUnderSessionLock(ctx, key, seed, func(ctx context.Context, tx *Tx) error {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, key, seed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !free() {
		t.Fatal("a committed transaction left its session lock held")
	}
}
