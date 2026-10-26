package db

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

type DB struct {
	observer *queryObserver
	pool     *pgxpool.Pool
	// direct carries session state (see Direct); it is never used for ordinary queries.
	direct *pgxpool.Pool
	sql    *sql.DB
	q      *dbsqlc.Queries
	// migrationConfig opens the migrations' own pool on the direct DSN (no statement or lock timeouts).
	migrationConfig *pgxpool.Config

	serializable  transactionCounters
	readCommitted transactionCounters
}

type transactionCounters struct {
	calls     atomic.Uint64
	attempts  atomic.Uint64
	retries   atomic.Uint64
	exhausted atomic.Uint64
}

type TransactionStats struct {
	Calls     uint64 `json:"calls"`
	Attempts  uint64 `json:"attempts"`
	Retries   uint64 `json:"retries"`
	Exhausted uint64 `json:"exhausted"`
}

type TransactionSnapshot struct {
	Serializable  TransactionStats `json:"serializable"`
	ReadCommitted TransactionStats `json:"read_committed"`
}

type Tx struct {
	tx pgx.Tx
	q  *dbsqlc.Queries
}

// directMaxConns bounds the direct pool: session state holds one connection each (the machine-events LISTEN, the
// worker leader, a team identity sync, each address reconcile), so it stays small.
const directMaxConns = 4

func Open(cfg config.Database) (*DB, error) {
	if cfg.Driver != "postgres" && cfg.Driver != "pgx" {
		return nil, fmt.Errorf("unsupported database driver %q: postgres is required for first release", cfg.Driver)
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("database dsn is required")
	}
	observer := &queryObserver{}
	poolConfig, err := newPoolConfig(cfg, cfg.DSN, cmp.Or(cfg.MaxConns, 20), observer)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	poolConfig.ConnConfig.Tracer = boundedAcquire{queryObserver: observer, timeout: cmp.Or(cfg.AcquireTimeout, 3*time.Second)}
	directConfig, err := newPoolConfig(cfg, cmp.Or(strings.TrimSpace(cfg.DirectDSN), cfg.DSN), directMaxConns, observer)
	if err != nil {
		return nil, fmt.Errorf("parse postgres direct dsn: %w", err)
	}
	// Migrations (index builds, table rewrites) run longer than any request; their pool has no time bounds, so the
	// timeouts that the role defaults or startup parameters set are cleared on every migration connection.
	migrationConfig := directConfig.Copy()
	migrationConfig.MaxConns = 2
	migrationConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET statement_timeout = 0; SET lock_timeout = 0; SET idle_in_transaction_session_timeout = 0`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	direct, err := pgxpool.NewWithConfig(context.Background(), directConfig)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open direct postgres: %w", err)
	}
	return &DB{observer: observer, pool: pool, direct: direct, sql: stdlib.OpenDBFromPool(pool), q: dbsqlc.New(pool), migrationConfig: migrationConfig}, nil
}

// newPoolConfig parses dsn for a pool. Its connections start with the session settings unless cfg says the role
// defaults provide them.
func newPoolConfig(cfg config.Database, dsn string, maxConns int32, observer *queryObserver) (*pgxpool.Config, error) {
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.SendsStartupParams() {
		for name, value := range sessionSettings(cfg) {
			poolConfig.ConnConfig.RuntimeParams[name] = value
		}
	}
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.MaxConns = maxConns
	poolConfig.ConnConfig.Tracer = observer
	return poolConfig, nil
}

// sessionSettings is the one list of settings every connection runs with. Open sends them as startup parameters
// and db-role-defaults stores them as role defaults, so the two cannot drift apart.
func sessionSettings(cfg config.Database) map[string]string {
	settings := map[string]string{
		"search_path": "paperboat",
		// JIT compilation costs ~1 s on a query like the machine inventory's (plpgsql lateral calls inflate its estimated
		// cost past jit_above_cost) and never pays off for this OLTP workload: the same statement runs in ~18 ms without it.
		"jit": "off",
	}
	for name, value := range map[string]time.Duration{
		"statement_timeout":                   cmp.Or(cfg.StatementTimeout, 30*time.Second),
		"lock_timeout":                        cmp.Or(cfg.LockTimeout, 10*time.Second),
		"idle_in_transaction_session_timeout": cmp.Or(cfg.IdleInTransactionTimeout, 60*time.Second),
	} {
		settings[name] = strconv.FormatInt(value.Milliseconds(), 10)
	}
	return settings
}

// SetRoleDefaults stores the session settings as defaults of the connecting role, so connections through a pooler
// that rejects startup parameters run with them too. It runs on the direct pool and needs no schema.
func (d *DB) SetRoleDefaults(ctx context.Context, cfg config.Database) error {
	settings := sessionSettings(cfg)
	tx, err := d.direct.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin role defaults: %w", err)
	}
	for _, name := range slices.Sorted(maps.Keys(settings)) {
		// Names are constants and values are constants or whole milliseconds, so neither needs quoting.
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER ROLE CURRENT_USER SET %s = '%s'", name, settings[name])); err != nil {
			return errors.Join(fmt.Errorf("set role default %s: %w", name, err), tx.Rollback(ctx))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit role defaults: %w", err)
	}
	return nil
}

func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	sqlErr := d.sql.Close()
	d.pool.Close()
	d.direct.Close()
	return sqlErr
}

func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.sql == nil {
		return errors.New("database is not open")
	}
	return d.pool.Ping(ctx)
}

func (d *DB) SQL() *sql.DB {
	if d == nil {
		return nil
	}
	return d.sql
}

func (d *DB) Queries() *dbsqlc.Queries {
	if d == nil {
		return nil
	}
	return d.q
}

func (d *DB) Pool() *pgxpool.Pool {
	if d == nil {
		return nil
	}
	return d.pool
}

// Direct is the pool for session state: a session advisory lock (pg_try_advisory_lock, pg_advisory_lock and their
// unlocks) and LISTEN must stay on one connection from acquire to release, and migrations need it too. Behind a
// transaction-pooling PgBouncer the unlock or the LISTEN would run on another backend, so the lock leaks and no
// notification arrives. Transaction-scoped locks (pg_advisory_xact_lock) and everything else use Pool or SQL.
func (d *DB) Direct() *pgxpool.Pool {
	if d == nil {
		return nil
	}
	return d.direct
}

func (d *DB) TransactionStats() TransactionSnapshot {
	if d == nil {
		return TransactionSnapshot{}
	}
	return TransactionSnapshot{
		Serializable:  d.serializable.snapshot(),
		ReadCommitted: d.readCommitted.snapshot(),
	}
}

// InTx runs fn inside a SERIALIZABLE transaction. Because serializable
// isolation can abort concurrent transactions with a serialization failure
// (40001) or deadlock (40P01), fn may be retried from a clean transaction. All
// InTx callers are idempotent (idempotency keys / ON CONFLICT), so replaying an
// aborted attempt is safe.
func (d *DB) InTx(ctx context.Context, fn func(context.Context, *Tx) error) error {
	d.serializable.calls.Add(1)
	attempts := 0
	err := retrySerializable(ctx, func() error {
		attempts++
		d.serializable.attempts.Add(1)
		if attempts > 1 {
			d.serializable.retries.Add(1)
		}
		return d.runInTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
	})
	if attempts >= serializableAttempts && isSerializationFailure(err) {
		d.serializable.exhausted.Add(1)
	}
	return err
}

// serializableAttempts bounds how often a serializable transaction is replayed after a
// serialization failure (40001) or deadlock (40P01).
const serializableAttempts = 10

// RetrySerializable runs attempt, which must open, use and finish its own serializable transaction (for code
// that uses BeginSerializable), and replays it from scratch after a serialization failure or deadlock, with
// bounded jittered backoff. Without it, one transient conflict (two servers overlapping during a rollout, a
// concurrent write) becomes a 503 and the client backs off for minutes. attempt must be safe to replay.
func RetrySerializable(ctx context.Context, attempt func() error) error {
	return retrySerializable(ctx, attempt)
}

func retrySerializable(ctx context.Context, attempt func() error) error {
	for n := 1; ; n++ {
		err := attempt()
		if err == nil || n >= serializableAttempts || !isSerializationFailure(err) {
			return err
		}
		// Exponential jitter prevents synchronized workers from repeatedly
		// colliding on the same serializable rows. The delay remains bounded.
		backoff := 5 * time.Millisecond << min(n-1, 5)
		if backoff > 200*time.Millisecond {
			backoff = 200 * time.Millisecond
		}
		half := backoff / 2
		jitter := time.Duration(time.Now().UnixNano() % int64(half+1))
		timer := time.NewTimer(half + jitter)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// serializableSetup is run first in every serializable transaction. A sequential scan takes a predicate lock on
// the whole table (PostgreSQL docs, Serializable Isolation), and the planner picks one for every small table, which
// most of ours are; every concurrent write to such a table then conflicts with the transaction. Index scans lock
// only the rows and pages they read.
const serializableSetup = `SET LOCAL enable_seqscan TO off`

// BeginSerializable starts a serializable database/sql transaction with the same setup as InTx, for code that
// manages its own transaction. Prefer InTx; otherwise wrap the whole transaction in RetrySerializable.
func BeginSerializable(ctx context.Context, database *sql.DB) (*sql.Tx, error) {
	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, serializableSetup); err != nil {
		return nil, errors.Join(fmt.Errorf("set up serializable transaction: %w", err), tx.Rollback())
	}
	return tx, nil
}

// InReadOnlySnapshot runs reads that must agree with each other in one repeatable-read, read-only transaction.
// It takes no row locks and no serializable predicate locks, so frequently polled reads cannot become the
// pivot of another transaction's serialization failure. Never use it to decide a write.
func (d *DB) InReadOnlySnapshot(ctx context.Context, fn func(context.Context, *Tx) error) error {
	return d.runInTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// InReadCommittedTx is for bounded work-queue claims that use FOR UPDATE SKIP
// LOCKED and explicit row versions. Serializable predicate locking conflicts
// with concurrent skip-locked claimers even when their committed rows are
// disjoint; read committed preserves the row-lock contract for that pattern.
func (d *DB) InReadCommittedTx(ctx context.Context, fn func(context.Context, *Tx) error) error {
	const maxAttempts = 5
	d.readCommitted.calls.Add(1)
	for attempt := 1; ; attempt++ {
		d.readCommitted.attempts.Add(1)
		err := d.runInTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
		if err == nil || attempt >= maxAttempts || !isSerializationFailure(err) {
			if attempt >= maxAttempts && isSerializationFailure(err) {
				d.readCommitted.exhausted.Add(1)
			}
			return err
		}
		timer := time.NewTimer(time.Duration(attempt) * 5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
			d.readCommitted.retries.Add(1)
		}
	}
}

func (c *transactionCounters) snapshot() TransactionStats {
	return TransactionStats{
		Calls:     c.calls.Load(),
		Attempts:  c.attempts.Load(),
		Retries:   c.retries.Load(),
		Exhausted: c.exhausted.Load(),
	}
}

// isSerializationFailure reports whether err is a Postgres serialization
// failure (40001) or deadlock (40P01) — both are safe to retry.
func isSerializationFailure(err error) (retryable bool) {
	defer func() {
		if recover() != nil {
			retryable = false
		}
	}()
	for depth := 0; err != nil && depth < 32; depth++ {
		value := reflect.ValueOf(err)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if value.IsNil() {
				return false
			}
		}
		if pgErr, ok := err.(*pgconn.PgError); ok {
			return pgErr.Code == "40001" || pgErr.Code == "40P01"
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapper.Unwrap()
	}
	return false
}

// Failed rollback leaves cleanup unconfirmed. Retain both failures without
// formatting the callback's potentially private error or authorizing a retry.
type transactionCleanupFailure struct {
	operation error
	rollback  error
}

func (*transactionCleanupFailure) Error() string { return "database transaction cleanup failed" }
func (e *transactionCleanupFailure) Unwrap() []error {
	return []error{e.operation, e.rollback}
}

// rollbackTimeout bounds the rollback of a failed transaction, which does not use the request's context.
const rollbackTimeout = 5 * time.Second

// rollbackFailedTx ends a transaction whose callback failed. The request's context is not used: a canceled or
// timed-out request is exactly when callbacks fail, and a rollback under that context fails too, destroying a
// healthy connection and hiding the callback's error behind transactionCleanupFailure (101 edge 503s with a
// connLockError, 2026-10-23..26). A connection already closed needs no rollback: the server ended the transaction
// with its session, and nothing was committed, so the callback's own error stays the one reported.
func rollbackFailedTx(tx pgx.Tx) error {
	closed := tx.Conn().IsClosed()
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !closed && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

func (d *DB) runInTx(ctx context.Context, options pgx.TxOptions, fn func(context.Context, *Tx) error) error {
	tx, err := d.pool.BeginTx(ctx, options)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	wrapped := &Tx{tx: tx, q: dbsqlc.New(tx)}
	setup := `SET LOCAL search_path TO paperboat`
	if options.IsoLevel == pgx.Serializable {
		setup += "; " + serializableSetup
	}
	if _, err := wrapped.Exec(ctx, setup); err != nil {
		if rollbackErr := rollbackFailedTx(tx); rollbackErr != nil {
			return &transactionCleanupFailure{operation: err, rollback: rollbackErr}
		}
		return fmt.Errorf("set transaction search path: %w", err)
	}
	if err := fn(ctx, wrapped); err != nil {
		if rollbackErr := rollbackFailedTx(tx); rollbackErr != nil {
			return &transactionCleanupFailure{operation: err, rollback: rollbackErr}
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func (tx *Tx) Queries() *dbsqlc.Queries {
	if tx == nil {
		return nil
	}
	return tx.q
}

func (tx *Tx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	return tx.tx.Exec(ctx, query, args...)
}

func (tx *Tx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return tx.tx.QueryRow(ctx, query, args...)
}

func (tx *Tx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	return tx.tx.Query(ctx, query, args...)
}
