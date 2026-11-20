package publicationdns

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A self-hosted workspace's names point at every ready node with a verified address in its pair's regions (edges do not
// forward, so a visitor may land on any), not one per failure domain and at most two: three nodes in the primary
// region, one in the backup region, and none from a region outside the pair or a node that is draining.
func TestWorkspaceTargetsListEveryNodeOfThePairRegions(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := t.Context()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	account := "pubdns_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`,
			`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`,
			`DELETE FROM paperboat.selfhost_installations WHERE scope_id=$1`,
			`DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $2`,
			`DELETE FROM paperboat.users WHERE id=$1`,
		} {
			args := []any{account}
			if strings.Contains(q, "$2") {
				args = append(args, "pubdns_%"+stamp)
			}
			_, _ = database.SQL().ExecContext(ctx, q, args...)
		}
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE roles @> ARRAY['edge'] AND id NOT LIKE $1`, "pubdns_%"+stamp)
	var installations []string
	addresses := map[string]string{}
	add := func(name, region, address, state string) {
		id := "pubdns_" + name + "_" + stamp
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,tls_spki_sha256,public_ingress_ipv4,public_ingress_verified_at)
VALUES($1,'default','1.0','epoch',$4,$4='ready',now(),$2,'one-domain',ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',decode(repeat('ab',32),'hex'),$3::inet,now())`, id, region, address, state)
		exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(md5($1||'k')||md5($1||'kk'),'hex'),decode(md5($1||'h')||md5($1||'hh'),'hex'),'fixture',$1,'tunnel','account',$3,$1,true)`, "inst_"+id, id, account)
		installations = append(installations, "inst_"+id)
		addresses[name] = address
	}
	add("a1", "r1_"+stamp, "192.0.2.1", "ready")
	add("a2", "r1_"+stamp, "192.0.2.2", "ready")
	add("a3", "r1_"+stamp, "192.0.2.3", "ready")
	add("b1", "r2_"+stamp, "192.0.2.4", "ready")
	add("a4", "r1_"+stamp, "192.0.2.5", "draining")
	add("c1", "r3_"+stamp, "192.0.2.6", "ready")
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',$2)`, account, installations)
	exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'self-hosted-only',$2)`, account, []string{"r1_" + stamp, "r2_" + stamp})
	var targets []string
	err = database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var inner error
		targets, inner = WorkspaceTargetsTx(ctx, tx, account, sql.NullString{}, "")
		return inner
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"}
	slices.Sort(targets)
	if !slices.Equal(targets, want) {
		t.Fatalf("targets=%v, want every ready node of the pair regions %v (more than two, none outside the pair, none draining)", targets, want)
	}
	if len(targets) > runtimewire.MaximumRegionCarriers {
		t.Fatalf("%d targets exceed the cap", len(targets))
	}
}
