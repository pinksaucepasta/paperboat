package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestTunnelEdgeObsoleteDrainCompletesDeletionOnPostgres(t *testing.T) {
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
	originalNow := now
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("drain_%d", now.UnixNano()), now)
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
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	if _, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	assignment := f.loadAssignment(t, database, 1)
	exec(`UPDATE paperboat.tunnels SET desired_state='deleted',deleted_at=$2 WHERE id=$1`, f.tunnelID, now)
	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.deleted','tunnel',$3,'draining','running',60,'changed',$1)`, "op_"+f.suffix, f.accountID, f.tunnelID)
	check := func(wantAssignment, wantOperation string) {
		t.Helper()
		if _, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
			t.Fatal(err)
		}
		row := f.loadAssignment(t, database, 1)
		var state string
		if err := database.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.operations WHERE id=$1`, "op_"+f.suffix).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if row.state != wantAssignment || state != wantOperation {
			t.Fatalf("assignment=%s operation=%s want %s/%s", row.state, state, wantAssignment, wantOperation)
		}
	}
	check("draining", "running")
	// A healthy epoch still owes an exact drain acknowledgement after the grace.
	now = originalNow.Add(connectorprotocol.IngressAuthorityLifetime + connectorprotocol.MaxClockSkew)
	check("draining", "running")
	// Replacement is fenced immediately, but retained decision lifetimes still
	// bound when unacknowledgeable cleanup may finish.
	now = originalNow
	exec(`UPDATE paperboat.control_tunnel_nodes SET process_epoch=$2 WHERE id=$1`, f.nodeID, f.epoch+"_replacement")
	check("draining", "running")
	now = originalNow.Add(connectorprotocol.IngressAuthorityLifetime + connectorprotocol.MaxClockSkew)
	check("detached", "succeeded")
	check("detached", "succeeded")
	if err := service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(assignment, "ready")}); err == nil {
		t.Fatal("retired assignment regained serving authority")
	}
}
