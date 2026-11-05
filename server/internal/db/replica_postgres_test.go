package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Reads go to the replica only while it is in rotation and has replayed past the client's position; otherwise,
// or with no replica configured, they go to the primary. The test points the replica DSN at the primary, which
// is always caught up.
func TestReadPoolHonoursRotationAndCommitPositionOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if plain.ReadPool(ctx, "") != plain.Pool() {
		t.Fatal("without a replica, reads must use the primary")
	}
	d, err := Open(config.Database{Driver: "postgres", DSN: dsn, ReplicaDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	position, err := d.CommitPosition(ctx)
	if err != nil || position == "" {
		t.Fatalf("commit position: %q %v", position, err)
	}
	if d.ReadPool(ctx, position) != d.Pool() {
		t.Fatal("a replica not yet in rotation served a read")
	}
	now := time.Now()
	d.replica.observe(0, true, now.Add(-time.Minute))
	d.replica.observe(0, true, now)
	if d.ReadPool(ctx, position) != d.replica.pool || d.ReadPool(ctx, "") != d.replica.pool {
		t.Fatal("a caught-up replica in rotation did not serve the read")
	}
	if d.ReadPool(ctx, "FFFFFFFF/FFFFFFFF") != d.Pool() {
		t.Fatal("a replica behind the client's position served the read")
	}
	lagCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	d.RunReplicaLag(lagCtx) // one real sample: the primary-as-replica has no lag
	cancel()
	if !d.replica.available() {
		t.Fatal("a real zero-lag sample took the replica out of rotation")
	}
}
