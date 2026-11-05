// Package servernodes keeps the shared record of control-plane server nodes: every node heartbeats its own
// row, and health is read from the rows, so no node is special and losing one breaks nothing (PLAN 3b).
package servernodes

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const (
	// HeartbeatInterval is how often a node refreshes its row; HealthyWithin is how recent it must be.
	HeartbeatInterval = 10 * time.Second
	HealthyWithin     = 30 * time.Second
)

// Node is this process's identity in the record.
type Node struct {
	ID, Region, Version, Commit string
	StartedAt                   time.Time
}

// Status is one node as any node's panel sees it.
type Status struct {
	ID, Region, State, Version, Commit string
	StartedAt, LastSeenAt              time.Time
	Healthy                            bool
}

// ErrRemoved: an operator removed this node; it must not rejoin by heartbeating.
var ErrRemoved = errors.New("this server node was removed")

// Heartbeat records the node as seen now. A removed node is never brought back by its own heartbeat.
func Heartbeat(ctx context.Context, d *db.DB, n Node) error {
	tag, err := d.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_nodes (id, region, version, commit, started_at, last_seen_at)
VALUES ($1,$2,$3,$4,$5,now())
ON CONFLICT (id) DO UPDATE SET region=excluded.region, version=excluded.version, commit=excluded.commit,
  started_at=excluded.started_at, last_seen_at=now()
WHERE control_server_nodes.state <> 'removed'`, n.ID, n.Region, n.Version, n.Commit, n.StartedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRemoved
	}
	return nil
}

// List returns every node that is not removed, healthy when seen within HealthyWithin and not draining.
func List(ctx context.Context, d *db.DB) ([]Status, error) {
	rows, err := d.Pool().Query(ctx, `SELECT id, region, state, version, commit, started_at, last_seen_at,
  state='active' AND last_seen_at > now() - make_interval(secs => $1)
FROM paperboat.control_server_nodes WHERE state <> 'removed' ORDER BY region, id`, HealthyWithin.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var s Status
		if err := rows.Scan(&s.ID, &s.Region, &s.State, &s.Version, &s.Commit, &s.StartedAt, &s.LastSeenAt, &s.Healthy); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Run heartbeats until ctx ends. Every node runs it (not a leader job). A failed beat is retried at the
// next tick; a removed node stops beating.
func Run(ctx context.Context, d *db.DB, n Node, failed func(error)) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		beat, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := Heartbeat(beat, d, n)
		cancel()
		if errors.Is(err, ErrRemoved) {
			failed(err)
			return
		}
		if err != nil && ctx.Err() == nil {
			failed(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
