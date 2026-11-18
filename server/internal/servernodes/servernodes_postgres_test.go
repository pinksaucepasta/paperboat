package servernodes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Two equal nodes record themselves; health comes from the shared rows; a stale node turns unhealthy, a
// draining one is not healthy, and a removed node cannot rejoin by heartbeating.
func TestServerNodesHeartbeatHealthAndRemovalOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	a := Node{ID: "node-a-" + suffix, Region: "mumbai", Version: "v1", Commit: "c1", StartedAt: time.Now().UTC()}
	b := Node{ID: "node-b-" + suffix, Region: "helsinki", Version: "v1", Commit: "c1", StartedAt: time.Now().UTC()}
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE id IN ($1,$2)`, a.ID, b.ID)
	})
	for _, n := range []Node{a, b} {
		if err := Heartbeat(ctx, store, n); err != nil {
			t.Fatal(err)
		}
	}
	// Walks every page two at a time, so paging (cursor, next_after, total) is exercised too.
	health := func() map[string]bool {
		out := map[string]bool{}
		after := ""
		for {
			page, err := List(ctx, store.Pool(), after, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Nodes) > 2 || page.Total < int64(len(out)+len(page.Nodes)) {
				t.Fatalf("page %+v after %d nodes", page, len(out))
			}
			for _, s := range page.Nodes {
				if s.ID <= after {
					t.Fatalf("page out of order: %q after %q", s.ID, after)
				}
				out[s.ID] = s.Healthy
			}
			if page.NextAfter == "" {
				return out
			}
			after = page.NextAfter
		}
	}
	if _, err := List(ctx, store.Pool(), "", 101); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("limit 101 err = %v", err)
	}
	if h := health(); !h[a.ID] || !h[b.ID] {
		t.Fatalf("fresh nodes not healthy: %v", h)
	}
	exec := func(q string, args ...any) {
		if _, err := store.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE paperboat.control_server_nodes SET last_seen_at=now()-interval '1 minute' WHERE id=$1`, a.ID)
	exec(`UPDATE paperboat.control_server_nodes SET state='draining' WHERE id=$1`, b.ID)
	if h := health(); h[a.ID] || h[b.ID] {
		t.Fatalf("a stale or draining node counted healthy: %v", h)
	}
	if err := Heartbeat(ctx, store, a); err != nil || !health()[a.ID] {
		t.Fatalf("a stale node did not recover by heartbeating: %v", err)
	}
	if err := Heartbeat(ctx, store, b); !errors.Is(err, ErrDraining) {
		t.Fatalf("a draining node's heartbeat did not report the drain: %v", err)
	}
	drained := make(chan struct{})
	var calls atomic.Int32
	runCtx, stop := context.WithCancel(ctx)
	go Run(runCtx, store, b, func(error) {}, func() { calls.Add(1); close(drained) })
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not report the drain")
	}
	stop()
	if calls.Load() != 1 {
		t.Fatalf("drained called %d times", calls.Load())
	}
	exec(`UPDATE paperboat.control_server_nodes SET state='removed' WHERE id=$1`, b.ID)
	if err := Heartbeat(ctx, store, b); !errors.Is(err, ErrRemoved) {
		t.Fatalf("a removed node rejoined by heartbeating: %v", err)
	}
	if _, listed := health()[b.ID]; listed {
		t.Fatal("a removed node is still listed")
	}
}

// The region map lists a region once one of its nodes serves: healthy while one is active, draining when its
// only fresh nodes drain; registered (no certificate yet), stale and removed nodes keep a region out. The
// cache keeps the last good map when the database fails.
func TestControlRegionMapOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprintf("r%d", time.Now().UnixNano()%1_000_000_000)
	nodes := map[string][2]string{ // id -> region, state
		p + "a-1": {p + "a", "active"}, p + "a-2": {p + "a", "draining"},
		p + "b-1": {p + "b", "draining"},
		p + "c-1": {p + "c", "registered"}, p + "d-1": {p + "d", "removed"}, p + "e-1": {p + "e", "active"},
	}
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE region LIKE $1`, p+"%")
	})
	for id, n := range nodes {
		seen := "now()"
		if n[0] == p+"e" {
			seen = "now()-interval '1 minute'"
		}
		if _, err := store.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_nodes (id, region, state, version, commit, started_at, last_seen_at)
VALUES ($1,$2,$3,'v','c',now(),`+seen+`)`, id, n[0], n[1]); err != nil {
			t.Fatal(err)
		}
	}
	mine := func(list []Region) map[string]Region {
		out := map[string]Region{}
		for _, r := range list {
			if strings.HasPrefix(r.ID, p) {
				out[r.ID] = r
			}
		}
		return out
	}
	cache := &RegionCache{DB: store, Domain: "example.test"}
	got := mine(cache.Get(ctx))
	if len(got) != 2 || got[p+"a"].State != "healthy" || got[p+"b"].State != "draining" || got[p+"a"].APIOrigin != "https://"+p+"a.example.test" {
		t.Fatalf("region map: %+v", got)
	}
	store.Close()
	cache.at = time.Time{}
	if again := mine(cache.Get(ctx)); len(again) != 2 {
		t.Fatalf("a database failure dropped the region map: %+v", again)
	}
}

// A drain is reversible while the node still beats: resume puts a draining node back to active (its next beat
// reports no drain). Once a node has beaten after the drain it has seen it and is shutting down, so resume says so
// (ErrStale); only a draining node resumes; an unknown node is not found.
func TestServerNodeResumeOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	n := Node{ID: fmt.Sprintf("resume-%d", time.Now().UnixNano()), Region: "mumbai", Version: "v1", Commit: "c1", StartedAt: time.Now().UTC()}
	t.Cleanup(func() { _, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE id=$1`, n.ID) })
	if err := Heartbeat(ctx, store, n); err != nil {
		t.Fatal(err)
	}
	if _, err := Resume(ctx, store, n.ID); !errors.Is(err, ErrState) {
		t.Fatalf("resuming an active node: %v", err)
	}
	if _, err := Drain(ctx, store, n.ID); err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	if err := store.Pool().QueryRow(ctx, `SELECT last_seen_at FROM paperboat.control_server_nodes WHERE id=$1`, n.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Heartbeat(ctx, store, n); !errors.Is(err, ErrDraining) {
		t.Fatalf("a draining node's beat: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT last_seen_at FROM paperboat.control_server_nodes WHERE id=$1`, n.ID).Scan(&after); err != nil || !after.Equal(before) {
		t.Fatalf("a draining beat moved last_seen_at from %v to %v (%v)", before, after, err)
	}
	got, err := Resume(ctx, store, n.ID)
	if err != nil || got.State != "active" {
		t.Fatalf("resume a draining node: %+v %v", got, err)
	}
	if err := Heartbeat(ctx, store, n); err != nil {
		t.Fatalf("a resumed node's beat still reports the drain: %v", err)
	}
	if _, err := Drain(ctx, store, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE paperboat.control_server_nodes SET last_seen_at=now()-interval '9 seconds' WHERE id=$1`, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Resume(ctx, store, n.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("resuming a drained node that stopped beating: %v", err)
	}
	if _, err := Remove(ctx, store, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Resume(ctx, store, n.ID); !errors.Is(err, ErrState) {
		t.Fatalf("resuming a removed node: %v", err)
	}
	if _, err := Resume(ctx, store, n.ID+"-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resuming an unknown node: %v", err)
	}
}
