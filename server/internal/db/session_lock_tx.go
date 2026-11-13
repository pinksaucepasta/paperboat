package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionLockWait bounds how long InTxUnderSessionLock waits for its lock.
const sessionLockWait = 30 * time.Second

// ErrSessionLockBusy means another transaction held the lock for all of sessionLockWait.
var ErrSessionLockBusy = errors.New("database: the lock stayed held by another transaction; retry shortly")

// InTxUnderSessionLock runs fn like InTx, holding the session advisory lock hashtextextended(key, seed) for every
// attempt. The lock is taken before the serializable transaction begins, because a SERIALIZABLE transaction's
// snapshot is taken at the start of its first statement (PostgreSQL docs, Transaction Isolation, 13.2.3): a lock
// taken inside it, such as planlimits' pg_advisory_xact_lock, is waited for after the snapshot, so a waiter decides
// on rows its predecessor already changed and SSI cancels it as a pivot. Waiting outside the transaction makes
// those transactions queue instead. The same session may take the same key again as a transaction lock inside fn.
//
// The lock is a session lock, so it runs on the direct pool (behind a transaction pooler its unlock would run on
// another server connection). Waiting polls pg_try_advisory_lock with backoff and holds no connection between tries;
// it ends at ctx or sessionLockWait. The lock is released on every exit; a connection whose unlock fails, or that a
// panic left inside a transaction, is closed instead of returned to the pool.
func (d *DB) InTxUnderSessionLock(ctx context.Context, key string, seed int64, fn func(context.Context, *Tx) error) error {
	return d.underSessionLock(ctx, key, seed, pgx.Serializable, &d.serializable, fn)
}

// InReadCommittedTxUnderSessionLock is InTxUnderSessionLock at READ COMMITTED, for a transaction that only takes
// row locks and atomic updates on rows other transactions share (a workspace's bandwidth period): at serializable
// those concurrent updates abort each other with 40001 instead of queuing on the row lock. The caller must not
// rely on a snapshot taken before its first lock.
func (d *DB) InReadCommittedTxUnderSessionLock(ctx context.Context, key string, seed int64, fn func(context.Context, *Tx) error) error {
	return d.underSessionLock(ctx, key, seed, pgx.ReadCommitted, &d.readCommitted, fn)
}

func (d *DB) underSessionLock(ctx context.Context, key string, seed int64, iso pgx.TxIsoLevel, counters *transactionCounters, fn func(context.Context, *Tx) error) error {
	conn, err := d.acquireSessionLock(ctx, key, seed)
	if err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			discardSessionConn(conn) // a panic in fn: its transaction may still be open
		}
	}()
	counters.calls.Add(1)
	attempts := 0
	err = retrySerializable(ctx, func() error {
		attempts++
		counters.attempts.Add(1)
		if attempts > 1 {
			counters.retries.Add(1)
		}
		return runInTxOn(ctx, conn, pgx.TxOptions{IsoLevel: iso}, fn)
	})
	if attempts >= serializableAttempts && isSerializationFailure(err) {
		counters.exhausted.Add(1)
	}
	released = true
	releaseSessionLock(conn, key, seed)
	return err
}

func (d *DB) acquireSessionLock(ctx context.Context, key string, seed int64) (*pgxpool.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionLockWait)
	defer cancel()
	for n := 0; ; n++ {
		conn, err := d.direct.Acquire(ctx)
		if err != nil {
			return nil, lockWaitError(ctx, err)
		}
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,$2))`, key, seed).Scan(&locked); err != nil {
			discardSessionConn(conn) // the lock may have been granted before the error
			return nil, lockWaitError(ctx, err)
		}
		if locked {
			return conn, nil
		}
		conn.Release()
		timer := time.NewTimer(min(5*time.Millisecond<<min(n, 5), 200*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, lockWaitError(ctx, ctx.Err())
		case <-timer.C:
		}
	}
}

func lockWaitError(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded) {
		return ErrSessionLockBusy
	}
	return fmt.Errorf("take session lock: %w", err)
}

// releaseSessionLock unlocks with its own bounded context (the request may be cancelled) and returns the connection
// to the pool only when the unlock is confirmed; otherwise it closes the connection, which releases the lock too, so
// the transaction's own result stands either way.
func releaseSessionLock(conn *pgxpool.Conn, key string, seed int64) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	var unlocked bool
	err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,$2))`, key, seed).Scan(&unlocked)
	if err != nil || !unlocked || conn.Conn().PgConn().TxStatus() != 'I' {
		slog.Warn("session lock release failed; its connection was closed instead", "unlocked", unlocked, "error", err)
		discardSessionConn(conn)
		return
	}
	conn.Release()
}

// discardSessionConn closes the connection, which ends its session and every lock it holds.
func discardSessionConn(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = conn.Hijack().Close(ctx)
}
