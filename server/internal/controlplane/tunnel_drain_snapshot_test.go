package controlplane

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A draining managed edge keeps its tunnel routes until drain_deadline, because its runtime carriers
// stay valid until then; new placements stay blocked elsewhere.
func TestTunnelSnapshotKeepsRoutesOfADrainingNodeUntilItsDeadline(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil || !safeTunnelEdgeAcceptanceDSN(dsn) {
		t.Fatalf("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database: %v", err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("%d", time.Now().UnixNano()), now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_ = database.Close()
	})
	fixture.insertBase(t, database)
	fixture.insertEngine(t, database)
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	snapshot := func() int {
		t.Helper()
		rows, err := service.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, fixture.nodeID, fixture.epoch)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if got := snapshot(); got != 1 {
		t.Fatalf("ready node snapshot = %d routes, want 1", got)
	}
	drainUntil := func(deadline time.Time) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET state='draining', ready=false, drain_deadline=$2 WHERE id=$1`, fixture.nodeID, deadline); err != nil {
			t.Fatal(err)
		}
	}
	drainUntil(now.Add(10 * time.Minute))
	if got := snapshot(); got != 1 {
		t.Fatalf("draining node before its deadline snapshot = %d routes, want 1", got)
	}
	drainUntil(now.Add(-time.Second))
	if got := snapshot(); got != 0 {
		t.Fatalf("draining node after its deadline snapshot = %d routes, want 0", got)
	}
}
