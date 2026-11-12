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

// An edge's route snapshot runs the eligibility functions once per route. As SQL functions their nested calls were
// planned again on every call (~3-13 ms per route), so ~200 routes took seconds on prod and edges timed out
// (2026-11-12). Plans allow 1,000 tunnels per account: one account's 1,000 tunnels on one edge stay well under the
// edge's control deadline.
func TestEdgeRouteSnapshotOfAThousandTunnelsOnPostgres(t *testing.T) {
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
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("scale_%d", time.Now().UnixNano()), now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.endpoints WHERE account_id=$1`, f.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
		_ = database.Close()
	})
	f.insertBase(t, database)
	f.insertEngine(t, database)
	const tunnels = 1000
	// p names the parameters once; every statement reads them from it.
	const p = `WITH p(tun,acct,sfx,host,n,node,epoch,sess) AS (SELECT $1::text,$2::text,$3::text,$4::text,$5::int,$6::text,$7::text,$8::text) `
	for _, statement := range []string{
		`INSERT INTO paperboat.tunnels (id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at)
 SELECT tun||'_'||g,acct,'scale-'||g||'-'||sfx,'active','public',1,'endpoint_'||gen_random_uuid(),'https://scale-'||g||'-'||sfx||'.tunnels.example.test',host,acct,'pending',now(),now() FROM p,generate_series(2,p.n) g`,
		`INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at)
 SELECT 'rte_'||tun||'_'||g,tun||'_'||g,'web','http','exact','scale-'||g||'-'||sfx||'.example.test','/','http','127.0.0.1:3000',acct,acct,now(),now() FROM p,generate_series(2,p.n) g`,
		`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id)
 SELECT t.id,'tunnel',t.account_id,t.created_by_host_id,t.access_mode,'persistent',t.id FROM p JOIN paperboat.tunnels t ON t.account_id=p.acct AND t.id LIKE p.tun||'\_%'`,
		`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id)
 SELECT 'rte_'||tun||'_'||g,tun||'_'||g,'t-scale-'||g||'-'||sfx||'.tunnels.example.test','http','rte_'||tun||'_'||g FROM p,generate_series(2,p.n) g`,
		`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id)
 SELECT 'rte_'||tun||'_'||g,tun||'_'||g,node,epoch,sess FROM p,generate_series(2,p.n) g`,
	} {
		if _, err := database.SQL().ExecContext(ctx, p+statement, f.tunnelID, f.accountID, strings.ReplaceAll(f.suffix, "_", "-"), f.hostOne, tunnels, f.nodeID, f.epoch, f.carrierSession()); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
	if _, err := database.SQL().ExecContext(ctx, `ANALYZE paperboat.tunnels, paperboat.tunnel_routes, paperboat.endpoints, paperboat.endpoint_routes, paperboat.endpoint_edge_assignments`); err != nil {
		t.Fatal(err)
	}
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	var best time.Duration
	for range 3 { // the best of three: one slow run on a shared test host is not the plan
		started := time.Now()
		rows, err := service.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, f.nodeID, f.epoch)
		if err != nil || len(rows) != tunnels {
			t.Fatalf("edge snapshot = %d routes, %v; want %d", len(rows), err, tunnels)
		}
		if run := time.Since(started); best == 0 || run < best {
			best = run
		}
	}
	t.Logf("edge snapshot of %d tunnels: %v", tunnels, best)
	if best > 2*time.Second {
		t.Fatalf("edge snapshot of %d tunnels took %v, want under 2s", tunnels, best)
	}
}
