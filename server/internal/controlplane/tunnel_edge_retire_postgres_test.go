package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// A late heartbeat inside the connector lease and edge registry window keeps an active route: one
// stalled database pass used to retire it after 15 s, so the edge withdrew and drained the route
// (cutting transfers) and it came back under a new generation. A staged assignment still needs fresh
// heartbeats, and an expired lease retires an active one.
func TestActiveTunnelAssignmentSurvivesLateHeartbeatUntilLeaseExpiryOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("retire_%d", now.UnixNano()), now)
	t.Cleanup(func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
		database.Close()
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	f.insertBase(t, database)
	f.insertConnector(t, database, f.connectorOne, f.hostOne, f.sessionOne, 11)
	stage := &EdgeService{store: database, clock: func() time.Time { return now }}
	if _, err := stage.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	assignment := f.loadAssignment(t, database, 1)
	// The connector lease runs 45 s and the edge registry 1 minute from the last heartbeat.
	exec(`UPDATE paperboat.tunnel_connector_sessions SET last_heartbeat_at=$2,lease_deadline=$2::timestamptz+interval '45 seconds' WHERE id=$1`, f.sessionOne, now)
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=$2,registry_expires_at=$2::timestamptz+interval '1 minute' WHERE id=$1`, f.nodeID, now)
	state := func() string {
		t.Helper()
		var got string
		if err := database.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.tunnel_edge_route_assignments WHERE assignment_id=$1`, assignment.assignmentID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	retire := func(at time.Time) {
		t.Helper()
		if _, err := database.Queries().RetireUnavailableTunnelEdgeAssignmentsV1(ctx, at); err != nil {
			t.Fatal(err)
		}
	}

	// Staged, 20 s without heartbeats: retired, as before.
	retire(now.Add(20 * time.Second))
	if got := state(); got != "draining" {
		t.Fatalf("staged assignment with stale heartbeats: state=%s want draining", got)
	}

	// Active, 20 s without heartbeats but inside the lease: kept.
	exec(`UPDATE paperboat.tunnel_edge_route_assignments SET state='active',observed_state='ready',observed_at=$2 WHERE assignment_id=$1`, assignment.assignmentID, now)
	retire(now.Add(20 * time.Second))
	if got := state(); got != "active" {
		t.Fatalf("active assignment with a 20 s stale heartbeat inside its lease: state=%s want active", got)
	}

	// Active, past the connector lease: retired.
	retire(now.Add(46 * time.Second))
	if got := state(); got != "draining" {
		t.Fatalf("active assignment past its connector lease: state=%s want draining", got)
	}

	// Active, connector lease extended but the edge registry expired: retired.
	exec(`UPDATE paperboat.tunnel_edge_route_assignments SET state='active' WHERE assignment_id=$1`, assignment.assignmentID)
	exec(`UPDATE paperboat.tunnel_connector_sessions SET lease_deadline=$2 WHERE id=$1`, f.sessionOne, now.Add(10*time.Minute))
	retire(now.Add(61 * time.Second))
	if got := state(); got != "draining" {
		t.Fatalf("active assignment on an edge whose registry expired: state=%s want draining", got)
	}
}
