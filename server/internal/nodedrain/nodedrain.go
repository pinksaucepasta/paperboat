// Package nodedrain takes an edge or relay node out of service in two ways, for the operator (managed nodes) and for
// an owner (their own nodes). Drain stops new connections: placement, DNS and admission skip the node, and carriers
// already attached stay valid until the drain deadline. Drain and move also wakes every machine attached to the node
// so it re-attaches now, and lists the relay's accounts in relay_pool_handovers so their carriers close at the
// deadline and the clients reconnect through other relays.
// Prior art: tailscale's DERP "home region" move is client-driven after the server stops listing a node
// (derp/derphttp/derphttp_client.go); here the server also names the deadline and wakes the clients.
package nodedrain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
)

const (
	// DrainGrace: Drain stops new connections; existing ones finish before the node leaves.
	DrainGrace = 10 * time.Minute
	// MoveGrace: Drain and move; existing work moves now and the node leaves after this. It exceeds the wildcard
	// DNS TTL (60 s) plus the 10 s DNS loop, so a visitor holding the old address still reaches a carrier until it expires.
	MoveGrace = 90 * time.Second
)

var (
	ErrNotFound = errors.New("node not found")
	ErrRetired  = errors.New("node is retired")
)

// wakeBatch bounds how many machines one read and one topic write cover.
var wakeBatch = 1000

// Begin puts the node in state draining and not ready, with a deadline one grace away. Draining again keeps the
// existing deadline, except that Drain and move may shorten it. When move is set and the node is a relay, every
// account the relay currently serves gets a relay_pool_handovers row closing at the deadline.
func Begin(ctx context.Context, tx *db.Tx, node string, move bool) error {
	grace := DrainGrace
	if move {
		grace = MoveGrace
	}
	var state string
	var isRelay bool
	err := tx.QueryRow(ctx, `SELECT state,roles && ARRAY['relay','peer_relay']::text[] FROM paperboat.control_tunnel_nodes WHERE id=$1 FOR UPDATE`, node).Scan(&state, &isRelay)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == "retired" {
		return ErrRetired
	}
	if _, err = tx.Exec(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,
 drain_deadline=CASE WHEN drain_deadline IS NULL THEN now()+make_interval(secs=>$2::float8)
  WHEN $3 THEN LEAST(drain_deadline,now()+make_interval(secs=>$2::float8)) ELSE drain_deadline END,
 version=version+1,updated_at=now() WHERE id=$1`, node, grace.Seconds(), move); err != nil {
		return fmt.Errorf("drain node: %w", err)
	}
	if !move || !isRelay {
		return nil
	}
	// The subject set of the node's current process is the list of accounts it serves.
	_, err = tx.Exec(ctx, `INSERT INTO paperboat.relay_pool_handovers(node_id,account_id,close_at)
 SELECT DISTINCT n.id,u.id,n.drain_deadline FROM paperboat.control_tunnel_nodes n
 JOIN paperboat.relay_subject_sets s ON s.node_id=n.id AND s.process_epoch=n.process_epoch AND s.node_generation=n.node_generation
 CROSS JOIN LATERAL jsonb_array_elements(s.subjects) e
 JOIN paperboat.users u ON u.id=e->>'account_id'
 WHERE n.id=$1
 ON CONFLICT (node_id,account_id) DO UPDATE SET close_at=LEAST(paperboat.relay_pool_handovers.close_at,excluded.close_at)`, node)
	if err != nil {
		return fmt.Errorf("list relay handovers: %w", err)
	}
	return nil
}

// Resume returns a drained node to registration (it becomes ready again on its next heartbeat) and cancels its handovers.
func Resume(ctx context.Context, tx *db.Tx, node string) error {
	tag, err := tx.Exec(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='registered',ready=false,drain_deadline=NULL,version=version+1,updated_at=now() WHERE id=$1 AND state<>'retired'`, node)
	if err != nil {
		return fmt.Errorf("resume node: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `DELETE FROM paperboat.relay_pool_handovers WHERE node_id=$1`, node); err != nil {
		return fmt.Errorf("cancel relay handovers: %w", err)
	}
	return nil
}

// Wake tells every machine holding a live carrier on the node to look at its tunnels and previews again, so it
// re-attaches now instead of when its attachment expires. Call it after Begin has committed.
func Wake(ctx context.Context, pool *pgxpool.Pool, node string) error {
	after := ""
	for {
		rows, err := pool.Query(ctx, `SELECT DISTINCT machine_id FROM paperboat.runtime_carrier_attachments WHERE edge_node_id=$1 AND expires_at>now() AND machine_id>$2 ORDER BY machine_id LIMIT $3`, node, after, wakeBatch)
		if err != nil {
			return fmt.Errorf("list machines to wake: %w", err)
		}
		machines := make([]string, 0, wakeBatch)
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
		if len(machines) < wakeBatch {
			return nil
		}
		after = machines[len(machines)-1]
	}
}
