package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat-server/internal/testutil"
)

func TestServingAssignmentAndDNSSurviveFailedCandidatePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("lkg_%d", time.Now().UnixNano()), now)
	f.insertBase(t, database)
	defer func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	}()
	f.insertConnector(t, database, f.connectorOne, f.hostOne, f.sessionOne, 11)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := testutil.EndpointLabel("tunnel-edge:"+f.suffix) + ".tunnels.example.test"
	// TLS pass-through isolates DNS readiness from certificate issuance.
	exec(`UPDATE paperboat.tunnel_routes SET protocol='tls',origin_scheme='tcp',match_type='managed',match_hostname=$2,path_prefix=NULL,preserve_host=false,host_override=NULL,public_tcp_listener_id='listener_tls_443',public_tcp_port=443 WHERE id=$1`, f.routeID, endpoint)
	exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_ipv4='8.8.8.8',public_ingress_verified_at=$2 WHERE id=$1`, f.nodeID, now)
	svc := &EdgeService{store: database, clock: func() time.Time { return now }}
	if n, err := svc.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || n != 1 {
		t.Fatalf("stage=%d err=%v", n, err)
	}
	initial := f.loadAssignment(t, database, 1)
	if err := svc.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(initial, "ready")}); err != nil {
		t.Fatal(err)
	}
	addresses := func(want int) {
		t.Helper()
		rows, err := database.Queries().ListReadyEdgeDNSAddressesV1(ctx, dbsqlc.ListReadyEdgeDNSAddressesV1Params{TunnelID: f.tunnelID, Hostname: endpoint, Now: sql.NullTime{Time: now, Valid: true}})
		if err != nil || len(rows) != want {
			t.Fatalf("DNS addresses=%d want=%d err=%v", len(rows), want, err)
		}
	}
	addresses(1)
	fault := f.routeID + "_fault"
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,public_tcp_listener_id,public_tcp_port,preserve_host) VALUES($1,$2,'fault','tcp','managed',$3,NULL,'tcp','127.0.0.1:38144',$4,$4,$1||'_listener',23456,false)`, fault, f.tunnelID, endpoint, f.accountID)
	// Real route creation increments the resource revision; it must not revoke unchanged access.
	exec(`UPDATE paperboat.tunnels SET generation=generation+1 WHERE id=$1`, f.tunnelID)
	payload := []byte(fmt.Sprintf(`{"routes":[{"id":%q},{"id":%q}]}`, f.routeID, fault))
	digest := sha256.Sum256(payload)
	exec(`INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until) VALUES($1,2,1,$2,$3,'pending',$4,$5)`, f.tunnelID, digest[:], payload, f.accountID, now.Add(time.Hour))
	if _, err := svc.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var premature int
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.tunnel_edge_route_assignments WHERE route_id=$1`, fault).Scan(&premature); err != nil || premature != 0 {
		t.Fatalf("route absent from applied snapshot was staged: %d err=%v", premature, err)
	}
	exec(`UPDATE paperboat.tunnel_connectors SET last_applied_config_generation=2 WHERE id=$1`, f.connectorOne)
	exec(`UPDATE paperboat.tunnel_connector_sessions SET applied_config_generation=2 WHERE id=$1`, f.sessionOne)
	if _, err := svc.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if row := f.loadAssignment(t, database, 1); row.state != "active" {
		t.Fatalf("candidate retired LKG: %s", row.state)
	}
	rows, err := database.Queries().ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Params{EdgeNodeID: f.nodeID, EdgeProcessEpoch: f.epoch, Now: sql.NullTime{Time: now, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.AssignmentID == initial.assignmentID && row.State == "active" {
			found = true
		}
	}
	if !found {
		t.Fatal("serving assignment missing from edge snapshot")
	}
	addresses(1)
	var policyBefore, policyAfter int64
	if err := database.SQL().QueryRowContext(ctx, `SELECT viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1`, f.routeID).Scan(&policyBefore); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE paperboat.tunnels SET access_mode='private',generation=generation+1 WHERE id=$1`, f.tunnelID)
	addresses(0)
	exec(`UPDATE paperboat.tunnels SET access_mode='public',generation=generation+1 WHERE id=$1`, f.tunnelID)
	if err := database.SQL().QueryRowContext(ctx, `SELECT viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1`, f.routeID).Scan(&policyAfter); err != nil || policyAfter <= policyBefore {
		t.Fatalf("access revocation revision reused: before=%d after=%d err=%v", policyBefore, policyAfter, err)
	}
	addresses(0)
	exec(`UPDATE paperboat.tunnel_connectors SET desired_state='revoked',revoked_at=$2 WHERE id=$1`, f.connectorOne, now)
	addresses(0)
	if _, err := svc.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if row := f.loadAssignment(t, database, 1); row.state != "draining" {
		t.Fatal("revocation retained LKG")
	}
	// Route deletion waits for the removal config and drain, and never marks a
	// failed/superseded route creation successful.
	deleteID, createID := "op_delete_"+f.suffix, "op_create_"+f.suffix
	for id, kind := range map[string]string{deleteID: "route.delete", createID: "route.create"} {
		exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),$4,'route',$3,'draining','running',60,'changed',$1)`, id, f.accountID, fault, kind)
	}
	exec(`UPDATE paperboat.tunnel_routes SET deleted_at=$2,desired_state='deleted' WHERE id=$1`, fault, now.Add(time.Second))
	store, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	operation := func(id, want string) {
		t.Helper()
		op, err := store.GetOperation(ctx, f.accountID, id)
		if err != nil || op.State != want {
			t.Fatalf("route operation=%s want=%s err=%v", op.State, want, err)
		}
	}
	operation(deleteID, "running")
	exec(`UPDATE paperboat.tunnel_edge_route_assignments SET state='detached',observed_state='detached',released_at=$2 WHERE route_id=$1`, fault, now)
	operation(deleteID, "running")
	exec(`UPDATE paperboat.tunnel_config_generations SET activation_state='superseded',activated_at=NULL WHERE tunnel_id=$1`, f.tunnelID)
	removal := []byte(fmt.Sprintf(`{"routes":[{"id":%q}]}`, f.routeID))
	removalHash := sha256.Sum256(removal)
	exec(`INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,created_at,activated_at,retained_until) VALUES($1,3,2,$2,$3,'active',$4,$5,$5,$6)`, f.tunnelID, removalHash[:], removal, f.accountID, now.Add(2*time.Second), now.Add(time.Hour))
	operation(deleteID, "running")
	exec(`UPDATE paperboat.tunnels SET summary_code='ready' WHERE id=$1`, f.tunnelID)
	operation(deleteID, "succeeded")
	operation(createID, "failed")
	failed, err := store.GetOperation(ctx, f.accountID, createID)
	if err != nil || failed.ErrorCode.String != "route_deleted" {
		t.Fatalf("deleted create failure code: %v", err)
	}
	if _, err := store.GetOperation(ctx, "other_account", deleteID); err == nil {
		t.Fatal("cross-account operation accepted")
	}

}

// `route add --wait` polls the operation until it succeeded, but nothing marked a route.create or
// route.patch operation done: the tunnel was ready and the operation stayed running forever.
func TestRouteOperationSucceedsOnceTheEdgeServesTheCurrentRouteOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("routeop_%d", time.Now().UnixNano()), now)
	f.insertBase(t, database)
	defer func() {
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	}()
	f.insertConnector(t, database, f.connectorOne, f.hostOne, f.sessionOne, 11)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	svc := &EdgeService{store: database, clock: func() time.Time { return now }}
	if n, err := svc.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || n != 1 {
		t.Fatalf("stage=%d err=%v", n, err)
	}
	store, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"route.create", "route.patch"} {
		exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,outcome,correlation_id) VALUES($1,$2,$1,decode(repeat('00',32),'hex'),$4,'route',$3,'connecting','running',60,'changed',$1)`, "op_"+kind+"_"+f.suffix, f.accountID, f.routeID, kind)
	}
	state := func(kind string) string {
		t.Helper()
		op, err := store.GetOperation(ctx, f.accountID, "op_"+kind+"_"+f.suffix)
		if err != nil {
			t.Fatal(err)
		}
		return op.State
	}
	for _, kind := range []string{"route.create", "route.patch"} {
		if got := state(kind); got != "running" {
			t.Fatalf("%s while the assignment is only staged = %s", kind, got)
		}
	}
	initial := f.loadAssignment(t, database, 1)
	if err := svc.ObserveRoutes(ctx, f.nodeID, []RouteObservation{f.observation(initial, "ready")}); err != nil {
		t.Fatal(err)
	}
	// A patch after the edge went ready raises the route generation: it is done only when the edge serves that one.
	exec(`UPDATE paperboat.tunnel_routes SET generation=generation+1 WHERE id=$1`, f.routeID)
	if got := state("route.patch"); got != "running" {
		t.Fatalf("route.patch before the edge serves the new generation = %s", got)
	}
	exec(`UPDATE paperboat.tunnel_routes SET generation=generation-1 WHERE id=$1`, f.routeID)
	for _, kind := range []string{"route.create", "route.patch"} {
		if got := state(kind); got != "succeeded" {
			t.Fatalf("%s once the edge serves the route = %s", kind, got)
		}
	}
}
