package servernodes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
	health := func() map[string]bool {
		list, err := List(ctx, store.Pool())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, s := range list {
			out[s.ID] = s.Healthy
		}
		return out
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
