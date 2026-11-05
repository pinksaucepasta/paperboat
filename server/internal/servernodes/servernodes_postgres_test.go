package servernodes

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		list, err := List(ctx, store)
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
