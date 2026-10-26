package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
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
	f.insertEngine(t, database)
	exec(`INSERT INTO paperboat.machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) SELECT $1,id,installation_generation,$1,$3,$4,1 FROM paperboat.user_machines WHERE id=$2`, "renew_"+f.suffix, f.hostOne, now.Add(-time.Minute), now.Add(time.Hour))
	exec(`INSERT INTO paperboat.machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM paperboat.machine_control_renewals WHERE operation_id=$1`, "renew_"+f.suffix)

	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.create','tunnel',$3,'connecting','running',40,'changed',$1)`, "op_"+f.suffix, f.accountID, f.tunnelID)
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
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
	// A second live edge holds the route too but has not observed it yet: the hostname can resolve
	// to it, so the tunnel is not ready until it has (F8-06 first-request EOF).
	secondNode := f.nodeID + "_second"
	secondSession := fmt.Sprintf("session_00000000-0000-4000-8000-%012d", now.UnixNano()%1e12+1)
	exec(`INSERT INTO paperboat.control_tunnel_nodes SELECT (jsonb_populate_record(NULL::paperboat.control_tunnel_nodes, to_jsonb(n) || jsonb_build_object('id',$2::text,'process_epoch','epoch_second_'||$3::text,'endpoints_epoch','epoch_second_'||$3::text,'tunnel_endpoints_epoch','epoch_second_'||$3::text))).* FROM paperboat.control_tunnel_nodes n WHERE n.id=$1`, f.nodeID, secondNode, f.suffix)
	t.Cleanup(func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, secondNode)
	})
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active,agent_capabilities) SELECT route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,$2,'epoch_second_'||$3::text,public_identity_key,route_revision,$4,$4,admission,expires_at,true,now(),false,agent_capabilities FROM paperboat.runtime_carrier_attachments WHERE route_id=$1 AND edge_node_id=$5`, "runtime_"+f.suffix, secondNode, f.suffix, secondSession, f.nodeID)
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id) VALUES($1,$2,$3,$4,$5)`, f.routeID, f.tunnelID, secondNode, "epoch_second_"+f.suffix, secondSession)
	if err := service.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(f.nodeID, f.epoch, f.carrierSession(), "ready")}); err != nil {
		t.Fatal(err)
	}
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
		t.Fatal(err)
	}
	assert("pending", "running")
	if err := service.ObserveRoutes(ctx, secondNode, []RouteObservation{f.observation(secondNode, "epoch_second_"+f.suffix, secondSession, "ready")}); err != nil {
		t.Fatal(err)
	}
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
		t.Fatal(err)
	}
	assert("ready", "succeeded")
	// Edges report every few seconds: an unchanged summary leaves the tunnel row untouched (PLAN 3b idle writes),
	// and an operation started after the tunnel was already ready still completes.
	var steady, after string
	if err := database.SQL().QueryRowContext(ctx, `SELECT xmin::text FROM paperboat.tunnels WHERE id=$1`, f.tunnelID).Scan(&steady); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),'tunnel.patch','tunnel',$3,'connecting','running',40,'changed',$1)`, "op_patch_"+f.suffix, f.accountID, f.tunnelID)
	for range 3 {
		if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SQL().QueryRowContext(ctx, `SELECT xmin::text FROM paperboat.tunnels WHERE id=$1`, f.tunnelID).Scan(&after); err != nil || after != steady {
		t.Fatalf("3 unchanged health refreshes rewrote the tunnel (xmin %s -> %s, err %v)", steady, after, err)
	}
	var patchState string
	if err := database.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.operations WHERE id=$1`, "op_patch_"+f.suffix).Scan(&patchState); err != nil || patchState != "succeeded" {
		t.Fatalf("an operation started while ready = %s, %v; want succeeded", patchState, err)
	}
	exec(`DELETE FROM paperboat.endpoint_edge_assignments WHERE route_id=$1 AND edge_node_id=$2`, f.routeID, secondNode)

	// The host reports its local app: unreachable holds the tunnel in origin_unreachable (the create
	// operation stays succeeded), a stale report ages out, and a ready report clears it.
	repository, err := tunnelv1.NewRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	report := func(state, code string, observed time.Time) {
		t.Helper()
		if err := repository.RecordOriginHealth(ctx, f.accountID, f.hostOne, "cor_origin_"+f.suffix, []tunnelv1.OriginRouteReport{{TunnelID: f.tunnelID, RouteID: f.routeID, State: state, Code: code, ObservedAt: observed}}, now); err != nil {
			t.Fatal(err)
		}
		if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
			t.Fatal(err)
		}
	}
	report("unreachable", "refused", now.Add(-90*time.Second))
	assert("ready", "succeeded") // older than the refresh window: aged out
	report("unreachable", "refused", now)
	assert("origin_unreachable", "succeeded")
	report("unreachable", "refused", now.Add(time.Millisecond)) // the 30 s heartbeat repeat is not a new event
	assert("origin_unreachable", "succeeded")
	report("ready", "", now.Add(2*time.Millisecond))
	assert("ready", "succeeded")
	other := tunnelv1.OriginRouteReport{TunnelID: f.tunnelID, RouteID: "route_of_another_tunnel_" + f.suffix, State: "unreachable", Code: "refused", ObservedAt: now}
	if err := repository.RecordOriginHealth(ctx, f.accountID, f.hostOne, "cor_origin_"+f.suffix, []tunnelv1.OriginRouteReport{other}, now); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_route_origin_health WHERE route_id=$1`, other.RouteID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a route outside the tunnel was stored: %d %v", stored, err)
	}
	var unreachableLogs, recoveredLogs int
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE code='tunnel_origin_unreachable'),count(*) FILTER (WHERE code='tunnel_origin_recovered') FROM paperboat.tunnel_log_entries WHERE tunnel_id=$1`, f.tunnelID).Scan(&unreachableLogs, &recoveredLogs); err != nil || unreachableLogs != 1 || recoveredLogs != 1 {
		t.Fatalf("tunnel logs: unreachable=%d recovered=%d want 1 and 1 (repeats are not new events): %v", unreachableLogs, recoveredLogs, err)
	}
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
	exec(`UPDATE paperboat.endpoint_edge_assignments SET desired='removed',observed_ready=false WHERE endpoint_id=$1`, f.tunnelID)
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
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
	if err := tunnelv1.RefreshHealth(ctx, database.Pool(), f.accountID, []string{f.tunnelID}, now); err != nil {
		t.Fatal(err)
	}
	assertOperation(deleteID, "succeeded")

}
