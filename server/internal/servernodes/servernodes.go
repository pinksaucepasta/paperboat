// Package servernodes keeps the shared record of control-plane server nodes: every node heartbeats its own
// row, and health is read from the rows, so no node is special and losing one breaks nothing (PLAN 3b).
package servernodes

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	ID            string    `json:"id"`
	Region        string    `json:"region"`
	State         string    `json:"state"`
	PublicAddress string    `json:"public_address,omitempty"`
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	StartedAt     time.Time `json:"started_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	Healthy       bool      `json:"healthy"`
}

// ErrRemoved means an operator removed this node; it must not rejoin by heartbeating.
var ErrRemoved = errors.New("this server node was removed")

// ErrDraining means an operator drains this node: the beat is recorded, and the node shuts down gracefully.
var ErrDraining = errors.New("this server node is draining")

// Heartbeat records the node as seen now. A removed node is never brought back by its own heartbeat.
func Heartbeat(ctx context.Context, d *db.DB, n Node) error {
	var state string
	err := d.Pool().QueryRow(ctx, `INSERT INTO paperboat.control_server_nodes (id, region, version, commit, started_at, last_seen_at)
VALUES ($1,$2,$3,$4,$5,now())
ON CONFLICT (id) DO UPDATE SET region=excluded.region, version=excluded.version, commit=excluded.commit,
  started_at=excluded.started_at, last_seen_at=now()
WHERE control_server_nodes.state <> 'removed'
RETURNING state`, n.ID, n.Region, n.Version, n.Commit, n.StartedAt).Scan(&state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrRemoved
	case err != nil:
		return err
	case state == "draining":
		return ErrDraining
	}
	return nil
}

// List returns every node that is not removed, healthy when seen within HealthyWithin and not draining.
func List(ctx context.Context, pool *pgxpool.Pool) ([]Status, error) {
	rows, err := pool.Query(ctx, `SELECT id, region, state, coalesce(public_address, ''), version, commit, started_at, last_seen_at,
  state='active' AND last_seen_at > now() - make_interval(secs => $1)
FROM paperboat.control_server_nodes WHERE state <> 'removed' ORDER BY region, id`, HealthyWithin.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var s Status
		if err := rows.Scan(&s.ID, &s.Region, &s.State, &s.PublicAddress, &s.Version, &s.Commit, &s.StartedAt, &s.LastSeenAt, &s.Healthy); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Run heartbeats until ctx ends. Every node runs it (not a leader job). A failed beat is retried at the next
// tick; a removed node stops beating; drained is called once when an operator drains the node.
func Run(ctx context.Context, d *db.DB, n Node, failed func(error), drained func()) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	var drainOnce sync.Once
	for {
		beat, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := Heartbeat(beat, d, n)
		cancel()
		switch {
		case errors.Is(err, ErrRemoved):
			failed(err)
			return
		case errors.Is(err, ErrDraining):
			drainOnce.Do(drained)
		case err != nil && ctx.Err() == nil:
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
func Regions(ctx context.Context, pool *pgxpool.Pool, domain string) ([]Region, error) {
	rows, err := pool.Query(ctx, `SELECT region, bool_or(state='active') FROM paperboat.control_server_nodes
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
	if regions, err := Regions(read, c.DB.ReadPool(read, ""), c.Domain); err == nil {
		c.last, c.at = regions, time.Now()
	}
	if c.last == nil {
		return []Region{}
	}
	return c.last
}

var (
	ErrNotFound = errors.New("server node not found")
	ErrState    = errors.New("server node is not in a state this action applies to")
)

// Drain takes a node out of the region map and its region's DNS at once (no new clients); it keeps serving
// the connections it holds. Remove also refuses its heartbeat, so it cannot rejoin. The row is kept, so the
// region's DNS is still reconciled (to empty) after its last node leaves; the slot can be enrolled again.
func Drain(ctx context.Context, d *db.DB, id string) (Status, error) {
	return setState(ctx, d, id, "draining", "active", "registered")
}

func Remove(ctx context.Context, d *db.DB, id string) (Status, error) {
	return setState(ctx, d, id, "removed", "active", "registered", "draining")
}

func setState(ctx context.Context, d *db.DB, id, to string, from ...string) (Status, error) {
	var s Status
	err := d.Pool().QueryRow(ctx, `UPDATE paperboat.control_server_nodes SET state=$2 WHERE id=$1 AND state = ANY($3)
RETURNING id, region, state, coalesce(public_address, ''), version, commit, started_at, last_seen_at`, id, to, from).
		Scan(&s.ID, &s.Region, &s.State, &s.PublicAddress, &s.Version, &s.Commit, &s.StartedAt, &s.LastSeenAt)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Status{}, err
	}
	var exists bool
	if err := d.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.control_server_nodes WHERE id=$1)`, id).Scan(&exists); err != nil {
		return Status{}, err
	}
	if !exists {
		return Status{}, ErrNotFound
	}
	return Status{}, ErrState
}
