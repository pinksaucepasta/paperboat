package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// The database had no time bounds (Lane X robustness #3): a stuck query or a lock queue could hold a pool
// connection forever (a test INSERT ran 5.5 h), and the pool size was fixed. Every request connection carries the
// configured statement, lock and idle-in-transaction timeouts: a statement blocked on a lock fails with a clear
// lock_timeout error within its bound, a query on a full pool stops waiting at the acquire bound (F21-03), and
// migrations, on their own pool, are not cut by them.
func TestRequestConnectionsAreTimeBoundedOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn, MaxConns: 4, StatementTimeout: 2 * time.Second, LockTimeout: 500 * time.Millisecond, AcquireTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	if got := store.Pool().Config().MaxConns; got != 4 {
		t.Fatalf("pool size %d, want the configured 4", got)
	}
	var statement, lock, idle string
	if err := store.SQL().QueryRowContext(ctx, `SELECT current_setting('statement_timeout'),current_setting('lock_timeout'),current_setting('idle_in_transaction_session_timeout')`).Scan(&statement, &lock, &idle); err != nil {
		t.Fatal(err)
	}
	if statement != "2s" || lock != "500ms" || idle != "1min" {
		t.Fatalf("session timeouts %s/%s/%s, want 2s/500ms/1min", statement, lock, idle)
	}
	// A statement waiting on a lock gives up at the lock bound with lock_not_available.
	holder, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(ctx, `LOCK TABLE paperboat.goose_db_version IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = store.SQL().ExecContext(ctx, `SELECT count(*) FROM paperboat.goose_db_version`)
	if !isCode(err, "55P03") || time.Since(started) > 2*time.Second {
		t.Fatalf("a statement behind a held lock: %v after %v, want lock_not_available within the 500 ms bound", err, time.Since(started))
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A statement past its bound is cancelled by the server.
	if _, err := store.SQL().ExecContext(ctx, `SELECT pg_sleep(3)`); !isCode(err, "57014") {
		t.Fatalf("a 3 s statement under a 2 s bound: %v, want query_canceled", err)
	}
	// A full pool: once every connection is held, the wait for another ends at the acquire bound (the HTTP layer
	// answers 503 + Retry-After), not at the caller's deadline, and the next query succeeds as soon as one is free.
	var held []interface{ Release() }
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()
	for {
		started = time.Now()
		conn, err := store.Pool().Acquire(ctx)
		if err == nil {
			held = append(held, conn)
			if len(held) > 4 {
				t.Fatal("acquired more connections than the pool's 4")
			}
			continue
		}
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(started) > 3*time.Second {
			t.Fatalf("an acquire on a full pool: %v after %v, want the 1 s acquire bound (pool %+v)", err, time.Since(started), store.Pool().Stat())
		}
		break
	}
	if len(held) == 0 {
		t.Fatal("no connection was acquired before the bound")
	}
	held[0].Release()
	held = held[1:]
	if _, err := store.Pool().Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("after a connection was freed: %v", err)
	}
}

func isCode(err error, code string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == code
}
