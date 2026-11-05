// Package servernodes keeps the shared record of control-plane server nodes: every node heartbeats its own
// row, and health is read from the rows, so no node is special and losing one breaks nothing (PLAN 3b).
package servernodes

import (
	"context"
	"errors"
	"sync"
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
	ID         string    `json:"id"`
	Region     string    `json:"region"`
	State      string    `json:"state"`
	Version    string    `json:"version"`
	Commit     string    `json:"commit"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Healthy    bool      `json:"healthy"`
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

// Region is one entry of the control-plane region map in discovery (PLAN 3b): its API origin and state.
type Region struct {
	ID        string `json:"id"`
	APIOrigin string `json:"api_origin"`
	State     string `json:"state"` // healthy | draining
}

// Regions lists the regions with a fresh serving node: healthy when one is active, draining when the only
// fresh nodes drain. Registered nodes (no certificate yet) and stale or removed ones are not listed.
func Regions(ctx context.Context, d *db.DB, domain string) ([]Region, error) {
	rows, err := d.Pool().Query(ctx, `SELECT region, bool_or(state='active') FROM paperboat.control_server_nodes
WHERE state IN ('active','draining') AND last_seen_at > now() - make_interval(secs => $1)
GROUP BY region ORDER BY region`, HealthyWithin.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Region{}
	for rows.Next() {
		var r Region
		var active bool
		if err := rows.Scan(&r.ID, &active); err != nil {
			return nil, err
		}
		r.APIOrigin, r.State = "https://"+r.ID+"."+domain, "draining"
		if active {
			r.State = "healthy"
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegionCache answers discovery from memory; it refreshes at most every few seconds and keeps the last good
// map when the database fails, so discovery never breaks on a read error.
type RegionCache struct {
	DB     *db.DB
	Domain string
	mu     sync.Mutex
	at     time.Time
	last   []Region
}

func (c *RegionCache) Get(ctx context.Context) []Region {
	if c == nil || c.Domain == "" {
		return []Region{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < 5*time.Second && c.last != nil {
		return c.last
	}
	read, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if regions, err := Regions(read, c.DB, c.Domain); err == nil {
		c.last, c.at = regions, time.Now()
	}
	if c.last == nil {
		return []Region{}
	}
	return c.last
}
