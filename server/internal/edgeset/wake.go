package edgeset

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
)

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// WakeBatch bounds how many machines one wake-up read and one topic write cover.
var WakeBatch = 1000

// Wake wakes the workspace's machines on the topics their runtime watches for edge changes, so each looks at its
// attachment again instead of waiting for it to expire. Every path that stores a changed pair (a user's choice, a
// pool flip, the sweep, an operator re-placing a region) calls it right after the commit, outside the transaction. It pages through the machines by id, WakeBatch at a time.
func Wake(ctx context.Context, pool Querier, ws Scope) error {
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
