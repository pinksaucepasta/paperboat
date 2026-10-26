package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// The server runs behind a transaction-pooling PgBouncer, as on PlanetScale (F21-15, PLAN 3b "PlanetScale Postgres
// readiness"): with the pooled DSN through the bouncer and the direct DSN to PostgreSQL, startup parameters off and
// the session settings stored as role defaults, every pooled connection runs with search_path, jit and the timeouts;
// 300 distinct named prepared statements from 16 goroutines and serializable transactions work under transaction
// pooling; LISTEN and a session advisory lock live on the direct pool (a NOTIFY sent pooled wakes the listener, and the
// lock is held across pooled transactions); migrations run on the direct pool. Needs PAPERBOAT_TEST_PGBOUNCER_DSN
// (the bouncer, transaction mode) and PAPERBOAT_TEST_DATABASE_DSN (the same database, direct), with the app role.
func TestServerRunsBehindTransactionPoolingPgBouncerOnPostgres(t *testing.T) {
	pooled, direct := os.Getenv("PAPERBOAT_TEST_PGBOUNCER_DSN"), os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if pooled == "" || direct == "" {
		t.Skip("requires PgBouncer in transaction mode in front of isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(direct, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	off := false
	cfg := config.Database{Driver: "postgres", DSN: pooled, DirectDSN: direct, StartupParams: &off, MaxConns: 8,
		StatementTimeout: 7 * time.Second, LockTimeout: 3 * time.Second, IdleInTransactionTimeout: 40 * time.Second}
	setup, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.SetRoleDefaults(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_ = setup.Close()
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatalf("migrations on the direct pool: %v", err)
	}
	var path, jit, statement, lock, idle string
	if err := store.SQL().QueryRowContext(ctx, `SELECT current_setting('search_path'),current_setting('jit'),current_setting('statement_timeout'),
 current_setting('lock_timeout'),current_setting('idle_in_transaction_session_timeout')`).Scan(&path, &jit, &statement, &lock, &idle); err != nil {
		t.Fatal(err)
	}
	if path != "paperboat" || jit != "off" || statement != "7s" || lock != "3s" || idle != "40s" {
		t.Fatalf("pooled session settings %s/%s/%s/%s/%s, want the role defaults paperboat/off/7s/3s/40s", path, jit, statement, lock, idle)
	}
	// Named prepared statements (pgx's statement cache) under transaction pooling, concurrently.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				var n int
				query := fmt.Sprintf(`SELECT $1::int + %d FROM paperboat.goose_db_version LIMIT 1`, i)
				if err := store.Pool().QueryRow(ctx, query, g).Scan(&n); err != nil || n != g+i {
					errs <- fmt.Errorf("statement %d on goroutine %d: %d, %v", i, g, n, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Serializable transactions with retry, pooled.
	for range 20 {
		if err := store.InTx(ctx, func(ctx context.Context, tx *Tx) error {
			_, err := tx.Exec(ctx, `SELECT count(*) FROM paperboat.goose_db_version`)
			return err
		}); err != nil {
			t.Fatalf("serializable transaction through the bouncer: %v", err)
		}
	}
	// LISTEN on the direct pool, NOTIFY pooled.
	listener, err := store.Direct().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `LISTEN pgbouncer_rehearsal`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `SELECT pg_notify('pgbouncer_rehearsal','ping')`); err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	notification, err := listener.Conn().WaitForNotification(waitCtx)
	waitCancel()
	if err != nil || notification.Payload != "ping" {
		t.Fatalf("a pooled NOTIFY did not reach the direct LISTEN: %v", err)
	}
	// A session advisory lock on the direct pool stays held across pooled transactions.
	if _, err := listener.Exec(ctx, `SELECT pg_advisory_lock(31415926)`); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		var held bool
		if err := store.Pool().QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(31415926)`).Scan(&held); err != nil || held {
			t.Fatalf("the direct pool's session lock was not held when seen pooled: %v %v", held, err)
		}
	}
	if _, err := listener.Exec(ctx, `SELECT pg_advisory_unlock(31415926)`); err != nil {
		t.Fatal(err)
	}
}
