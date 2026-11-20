package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// An edge's route report waits for a concurrent write to the assignment row it observes instead of failing with
// 40001 and replaying: every write of the report is fenced by its own WHERE (generation, epoch, session).
func TestEdgeRouteReportWaitsForAConcurrentAssignmentWritePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("obsconc_%d", now.UnixNano()), now)
	t.Cleanup(func() {
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
		database.Close()
	})
	f.insertBase(t, database)
	f.insertEngine(t, database)
	service := &EdgeService{store: database, clock: func() time.Time { return now }}

	writer, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE paperboat.endpoint_edge_assignments SET changed_at=changed_at WHERE route_id=$1`, f.routeID); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	before := database.TransactionStats()
	done := make(chan error, 1)
	go func() {
		done <- service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(f.nodeID, f.epoch, f.carrierSession(), "ready")})
	}()
	for waiting := false; !waiting; {
		select {
		case err := <-done:
			t.Fatalf("the report finished before the assignment write committed: %v", err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := database.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := database.TransactionStats()
	if r := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; r != 0 {
		t.Fatalf("the report retried %d transaction attempts (40001/40P01)", r)
	}
	var ready bool
	if err := database.SQL().QueryRowContext(ctx, `SELECT observed_ready FROM paperboat.endpoint_edge_assignments WHERE route_id=$1`, f.routeID).Scan(&ready); err != nil || !ready {
		t.Fatalf("observed_ready = %v, err %v", ready, err)
	}
}
