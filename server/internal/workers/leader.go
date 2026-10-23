package workers

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Leadership gates a Supervisor: Acquire blocks until this instance may run the workers (or ctx ends),
// lost closes when that right ends, and release must be called after every worker has returned.
type Leadership interface {
	Acquire(ctx context.Context) (lost <-chan struct{}, release func(), err error)
}

// workerLeaderLockKey is the session advisory lock that elects the one server instance running workers.
const workerLeaderLockKey int64 = 0x7062776c // "pbwl"

// PostgresLeader elects one worker leader among server instances on one database: a session advisory
// lock on a pinned connection, checked every Check by a heartbeat write to paperboat.worker_leader. When
// the connection fails, PostgreSQL drops the lock with the backend and the heartbeat failure ends this
// instance's term; a standby retries every Retry (jittered). Overlap is bounded by Check plus the
// heartbeat timeout, and every worker also claims its rows (SKIP LOCKED, leases), so a short overlap
// never doubles work.
// Prior art: Teleport lib/backend/lock.go:63-194 RunWhileLocked (lease refreshed at TTL/2, retry
// interval, context cancelled on loss), used by lib/auth/connector_monitor.go:141. Headscale runs one
// instance only.
type PostgresLeader struct {
	DB                *sql.DB
	Instance, Version string
	Retry, Check      time.Duration
	Logger            *slog.Logger
	// Mode reports "leader", "standby" or "drained" on every change (metrics).
	Mode func(string)
}

const (
	leaderStatementTimeout = 5 * time.Second
	// LeaderRetry and LeaderCheck bound a handover: a standby takes over within about
	// LeaderCheck+leaderStatementTimeout+LeaderRetry after the leader's connection or process dies.
	LeaderRetry = 5 * time.Second
	LeaderCheck = 5 * time.Second
)

func (l *PostgresLeader) Acquire(ctx context.Context) (<-chan struct{}, func(), error) {
	l.mode("standby")
	for {
		conn, err := l.tryLock(ctx)
		if conn != nil {
			l.Logger.Info("worker leader acquired", "instance", l.Instance, "version", l.Version)
			l.mode("leader")
			return l.hold(conn)
		}
		if err != nil && ctx.Err() == nil {
			l.Logger.Warn("worker leader election failed; retrying", "instance", l.Instance, "error", err)
		}
		wait := l.Retry/2 + rand.N(l.Retry/2+1)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// tryLock returns a pinned connection holding the lock, or nil when another instance leads.
func (l *PostgresLeader) tryLock(ctx context.Context) (*sql.Conn, error) {
	conn, err := l.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, leaderStatementTimeout)
	defer cancel()
	var ok bool
	if err = conn.QueryRowContext(qctx, `SELECT pg_try_advisory_lock($1)`, workerLeaderLockKey).Scan(&ok); err != nil || !ok {
		discard(conn)
		return nil, err
	}
	if _, err = conn.ExecContext(qctx, `INSERT INTO paperboat.worker_leader(id,instance,version,since,heartbeat_at)
 VALUES(true,$1,$2,now(),now()) ON CONFLICT(id) DO UPDATE SET instance=excluded.instance,version=excluded.version,
 since=excluded.since,heartbeat_at=excluded.heartbeat_at`, l.Instance, l.Version); err != nil {
		discard(conn)
		return nil, err
	}
	return conn, nil
}

func (l *PostgresLeader) hold(conn *sql.Conn) (<-chan struct{}, func(), error) {
	lost, stop, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(l.Check)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), leaderStatementTimeout)
			_, err := conn.ExecContext(ctx, `UPDATE paperboat.worker_leader SET heartbeat_at=now() WHERE id AND instance=$1`, l.Instance)
			cancel()
			if err != nil {
				l.Logger.Warn("worker leader connection lost; stopping workers", "instance", l.Instance, "error", err)
				close(lost)
				return
			}
		}
	}()
	release := func() {
		close(stop)
		<-stopped
		discard(conn)
		l.Logger.Info("worker leader released", "instance", l.Instance)
		l.mode("standby")
	}
	return lost, release, nil
}

// discard releases the lock and closes the physical connection instead of returning it to the pool, so a
// session advisory lock can never stay behind on a pooled connection. (Returning driver.ErrBadConn from
// Conn.Raw does not discard a sql.Conn: on 2026-10-23 the released lock stayed held by the draining server's
// pool and no instance ran workers until the backend was terminated.) Unlocking is best effort: if the
// connection is already dead, PostgreSQL dropped the lock with the backend.
func discard(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), leaderStatementTimeout)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, workerLeaderLockKey)
	_ = conn.Raw(func(driverConn any) error {
		if closer, ok := driverConn.(driver.Conn); ok {
			_ = closer.Close()
		}
		return nil
	})
	_ = conn.Close()
}

func (l *PostgresLeader) mode(m string) {
	if l.Mode != nil {
		l.Mode(m)
	}
}
