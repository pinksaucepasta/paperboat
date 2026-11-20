package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestEdgeRouteReportUsesTunnelThenAssignmentLockOrderPostgres(t *testing.T) {
	database := openControlPlaneTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("healthlock_%d", now.UnixNano()), now)
	t.Cleanup(func() {
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	})
	f.insertBase(t, database)
	f.insertEngine(t, database)
	service := &EdgeService{store: database, clock: func() time.Time { return now }}

	// Tunnel edits lock the tunnel first and then update its assignments.
	writer, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE tunnels SET updated_at=updated_at WHERE id=$1`, f.tunnelID); err != nil {
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
			t.Fatalf("report finished before the tunnel write committed: %v", err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := database.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Exec(ctx, `UPDATE endpoint_edge_assignments SET changed_at=changed_at WHERE route_id=$1 AND edge_node_id=$2`, f.routeID, f.nodeID); err != nil {
		t.Fatalf("tunnel edit could not update its assignment: %v", err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := database.TransactionStats()
	if n := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; n != 0 {
		t.Fatalf("report retried %d transaction attempts (40001/40P01)", n)
	}
	var summary string
	if err := database.Pool().QueryRow(ctx, `SELECT summary_code FROM tunnels WHERE id=$1`, f.tunnelID).Scan(&summary); err != nil || summary != "ready" {
		t.Fatalf("summary = %q, want ready, error %v", summary, err)
	}
}
