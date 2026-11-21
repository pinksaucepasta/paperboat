package edgeset

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
)

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// WakeBatch bounds how many machines one wake-up read and one topic write cover.
var WakeBatch = 1000

// wake wakes the workspace's machines on the topics their runtime watches for edge changes, so each looks at its
// attachment again instead of waiting for it to expire. DrainWakes calls it for each workspace queued by EnqueueWake. It pages through the machines by id, WakeBatch at a time.
func wake(ctx context.Context, pool Querier, ws Scope) error {
	after := ""
	for {
		rows, err := pool.Query(ctx, `SELECT id FROM paperboat.user_machines WHERE deleted_at IS NULL AND revoked_at IS NULL AND id>$3
 AND CASE WHEN $1='team' THEN owner_team_id=$2 ELSE user_id=$2 AND owner_team_id IS NULL END ORDER BY id LIMIT $4`, ws.Kind, ws.ID, after, WakeBatch)
		if err != nil {
			return fmt.Errorf("list machines to wake: %w", err)
		}
		machines := make([]string, 0, WakeBatch)
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			machines = append(machines, id)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return fmt.Errorf("list machines to wake: %w", err)
		}
		for _, topic := range []string{machineevents.TopicTunnels, machineevents.TopicPreviews} {
			if err = machineevents.Publish(ctx, pool, machines, topic); err != nil {
				return fmt.Errorf("wake machines on %s: %w", topic, err)
			}
		}
		if len(machines) < WakeBatch {
			return nil
		}
		after = machines[len(machines)-1]
	}
}

// EnqueueWake queues a wake of the workspace's machines in the caller's transaction, so it exists exactly when the
// change that needs it commits. It reads no machine: listing them inside a serializable transaction takes predicate
// locks that fail revokes (TestReconcileLeavesMachinesAloneAndARevokeCommitsOnPostgres).
func EnqueueWake(ctx context.Context, tx *db.Tx, scope Scope) error {
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_edge_wakes(scope_kind,scope_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, scope.Kind, scope.ID); err != nil {
		return fmt.Errorf("queue edge wake: %w", err)
	}
	return nil
}

// drainBatch bounds how many queued workspaces one drain transaction takes.
const drainBatch = 50

// DrainWakes wakes the machines of queued workspaces and removes their rows, drainBatch per read-committed
// transaction: a crash before commit leaves the rows for the next drain, and SKIP LOCKED keeps two drainers from
// waking one workspace twice. It returns how many workspaces it woke.
func DrainWakes(ctx context.Context, database *db.DB) (int, error) {
	woke := 0
	for {
		n := 0
		err := database.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			rows, err := tx.Query(ctx, `DELETE FROM workspace_edge_wakes w USING (SELECT scope_kind,scope_id FROM workspace_edge_wakes
 ORDER BY queued_at LIMIT $1 FOR UPDATE SKIP LOCKED) q WHERE w.scope_kind=q.scope_kind AND w.scope_id=q.scope_id RETURNING w.scope_kind,w.scope_id`, drainBatch)
			if err != nil {
				return err
			}
			var scopes []Scope
			for rows.Next() {
				var s Scope
				if err = rows.Scan(&s.Kind, &s.ID); err != nil {
					break
				}
				scopes = append(scopes, s)
			}
			rows.Close()
			if err == nil {
				err = rows.Err()
			}
			if err != nil {
				return err
			}
			for _, s := range scopes {
				if err = wake(ctx, tx, s); err != nil {
					return err
				}
			}
			n = len(scopes)
			return nil
		})
		if err != nil {
			return woke, fmt.Errorf("drain edge wakes: %w", err)
		}
		woke += n
		if n < drainBatch {
			return woke, nil
		}
	}
}

// DrainWakesLogged is DrainWakes for callers whose change is already committed: a failure leaves the rows queued for
// the sweep's next drain, so it is logged, not returned.
func DrainWakesLogged(ctx context.Context, database *db.DB) {
	if _, err := DrainWakes(ctx, database); err != nil {
		slog.ErrorContext(ctx, "edge_wake_drain_failed", "error", err)
	}
}
