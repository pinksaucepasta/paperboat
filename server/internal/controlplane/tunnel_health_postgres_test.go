package controlplane

import (
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelv1"
	"os"
	"testing"
	"time"
)

func TestTunnelHealthFollowsObservedRoutesOnPostgres(t *testing.T) {
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
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("health_%d", now.UnixNano()), now)
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
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, "env_"+f.hostOne, f.hostOne, f.accountID)
	f.insertConnector(t, database, f.connectorOne, f.hostOne, f.sessionOne, 11)
	exec(`INSERT INTO paperboat.machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) SELECT $1,id,installation_generation,$1,$3,$4,1 FROM paperboat.user_machines WHERE id=$2`, "renew_"+f.suffix, f.hostOne, now.Add(-time.Minute), now.Add(time.Hour))
	exec(`INSERT INTO paperboat.machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM paperboat.machine_control_renewals WHERE operation_id=$1`, "renew_"+f.suffix)
	exec(`INSERT INTO paperboat.tunnel_connector_credential_generations(connector_id,tunnel_id,generation,credential_reference,credential_thumbprint,verifier_public_key,state,valid_until,created_at) VALUES($2,$3,1,$1,$1,decode(repeat('00',32),'hex'),'active',$4,$5)`, "cred_"+f.suffix, f.connectorOne, f.tunnelID, now.Add(time.Hour), now.Add(-time.Minute))

	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.create','tunnel',$3,'connecting','running',40,'changed',$1)`, "op_"+f.suffix, f.accountID, f.tunnelID)
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	if _, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	assignment := f.loadAssignment(t, database, 1)
	assert := func(code, state string) {
		t.Helper()
		var gotCode, gotState string
		if err := database.SQL().QueryRowContext(ctx, `SELECT t.summary_code,o.state FROM paperboat.tunnels t JOIN paperboat.operations o ON o.resource_id=t.id WHERE t.id=$1 AND o.operation_type='tunnel.create'`, f.tunnelID).Scan(&gotCode, &gotState); err != nil {
			t.Fatal(err)
		}
		if gotCode != code || gotState != state {
			t.Fatalf("health=%s operation=%s want %s/%s", gotCode, gotState, code, state)
		}
	}
	assert("pending", "running")
	if err := service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(assignment, "ready")}); err != nil {
		t.Fatal(err)
	}
	assert("ready", "succeeded")
	for _, boundary := range []struct{ name, deny, restore, id string }{
		{"seat", `UPDATE paperboat.user_machines SET seat_state='released' WHERE id=$1`, `UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1`, f.hostOne},
		{"machine", `UPDATE paperboat.user_machines SET state='pending' WHERE id=$1`, `UPDATE paperboat.user_machines SET state='online' WHERE id=$1`, f.hostOne},
		{"environment", `UPDATE paperboat.control_environments SET desired_state='suspended' WHERE id=$1`, `UPDATE paperboat.control_environments SET desired_state='active' WHERE id=$1`, "env_" + f.hostOne},
		{"identity", `UPDATE paperboat.machine_control_sessions SET expires_at=issued_at+interval '1 second' WHERE machine_id=$1`, `UPDATE paperboat.machine_control_sessions SET expires_at=issued_at+interval '2 hours' WHERE machine_id=$1`, f.hostOne},
		{"credential", `UPDATE paperboat.tunnel_connector_credential_generations SET state='revoked',revoked_at=now() WHERE connector_id=$1`, `UPDATE paperboat.tunnel_connector_credential_generations SET state='active',revoked_at=NULL WHERE connector_id=$1`, f.connectorOne},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			exec(boundary.deny, boundary.id)
			if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
				t.Fatal(err)
			}
			assert("pending", "succeeded")
			exec(boundary.restore, boundary.id)
			if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
				t.Fatal(err)
			}
			assert("ready", "succeeded")
		})
	}

	if err := service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(assignment, "degraded")}); err != nil {
		t.Fatal(err)
	}
	assert("route_unavailable", "succeeded")
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assert("pending", "succeeded")
	exec(`UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, f.tunnelID)
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
		t.Fatal(err)
	}
	assert("paused", "succeeded")
	pauseID := "pause_" + f.suffix
	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.paused','tunnel',$3,'draining','running',60,'changed',$1)`, pauseID, f.accountID, f.tunnelID)
	assertOperation := func(id, want string) {
		t.Helper()
		var state string
		if err := database.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.operations WHERE id=$1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != want {
			t.Fatalf("operation state=%s want %s", state, want)
		}
	}
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
		t.Fatal(err)
	}
	assertOperation(pauseID, "running")
	exec(`UPDATE paperboat.tunnel_edge_route_assignments SET state='draining',observed_state='draining' WHERE tunnel_id=$1`, f.tunnelID)
	if err := service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(assignment, "detached")}); err != nil {
		t.Fatal(err)
	}
	assertOperation(pauseID, "succeeded")

	expiredID := "expired_" + f.suffix
	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.patch','tunnel',$3,'connecting','running',40,'changed',$1)`, expiredID, f.accountID, f.tunnelID)
	exec(`UPDATE paperboat.tunnels SET expires_at=$2 WHERE id=$1`, f.tunnelID, now.Add(time.Second))
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Expiry health is derived at read time; the sweeper owns its stored audit transition.
	assert("paused", "succeeded")
	assertOperation(expiredID, "failed")
	exec(`UPDATE paperboat.tunnels SET desired_state='deleted',deleted_at=$2 WHERE id=$1`, f.tunnelID, now)
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assert("deleted", "succeeded")
	deleteID := "delete_" + f.suffix
	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.deleted','tunnel',$3,'draining','running',60,'changed',$1)`, deleteID, f.accountID, f.tunnelID)
	if err := service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(assignment, "detached")}); err != nil {
		t.Fatal(err)
	}
	assertOperation(deleteID, "succeeded")

}
