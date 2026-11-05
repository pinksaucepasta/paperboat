package db

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Read replicas (PLAN 3b, the Aurora/PlanetScale follower-read model): a write answers with the primary's commit
// LSN (Paperboat-Commit-Position); a read that allows a replica is served there only once the replica has replayed
// at or past the position the client sent, otherwise by the primary. Authentication, authorization, grants,
// revocation, billing, idempotency and any read followed by a write never use ReadPool.

const (
	replicaMaxLag   = 2 * time.Second
	replicaRejoin   = 30 * time.Second
	replicaSampling = 5 * time.Second
)

type replicaState struct {
	pool *pgxpool.Pool

	mu         sync.Mutex
	inRotation bool
	underSince time.Time // first sample under the bound since it last went out
}

// observe applies one lag sample: above the bound (or a failed sample) the replica leaves rotation at once; it
// rejoins only after staying under the bound for replicaRejoin.
func (r *replicaState) observe(lag time.Duration, ok bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok || lag > replicaMaxLag {
		r.inRotation, r.underSince = false, time.Time{}
		return
	}
	if r.inRotation {
		return
	}
	if r.underSince.IsZero() {
		r.underSince = now
	}
	if now.Sub(r.underSince) >= replicaRejoin {
		r.inRotation = true
	}
}

func (r *replicaState) available() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inRotation
}

// replayedSQL is the replay position on a standby and the write position on a primary, so a replica DSN that
// points at a primary (tests, a region without a standby) is always caught up.
const replayedSQL = `SELECT CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END`

// RunReplicaLag samples the replica's replay lag every 5 s until ctx ends. Every instance runs it for its own pool.
func (d *DB) RunReplicaLag(ctx context.Context) {
	if d == nil || d.replica == nil {
		return
	}
	ticker := time.NewTicker(replicaSampling)
	defer ticker.Stop()
	for {
		sample, cancel := context.WithTimeout(ctx, 2*time.Second)
		var seconds float64
		err := d.replica.pool.QueryRow(sample, `SELECT CASE WHEN NOT pg_is_in_recovery() OR pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
  ELSE coalesce(extract(epoch FROM now() - pg_last_xact_replay_timestamp()), 3600) END`).Scan(&seconds)
		cancel()
		d.replica.observe(time.Duration(seconds*float64(time.Second)), err == nil, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// CommitPosition is the primary's current WAL position, sent to clients after a write.
func (d *DB) CommitPosition(ctx context.Context) (string, error) {
	var lsn string
	err := d.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn)
	return lsn, err
}

// ReadPool is the pool for a replica-safe read by a client that has seen position ("" = none): the replica when
// it is in rotation and has replayed past position, else the primary. Any doubt reads the primary.
func (d *DB) ReadPool(ctx context.Context, position string) *pgxpool.Pool {
	if d == nil || d.replica == nil || !d.replica.available() {
		return d.Pool()
	}
	if position == "" {
		return d.replica.pool
	}
	check, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var caughtUp bool
	if err := d.replica.pool.QueryRow(check, `SELECT (`+replayedSQL+`) >= $1::pg_lsn`, position).Scan(&caughtUp); err != nil || !caughtUp {
		return d.pool
	}
	return d.replica.pool
}
