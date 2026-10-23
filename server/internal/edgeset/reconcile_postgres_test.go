package edgeset

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func openIsolated(t *testing.T) *db.DB {
	t.Helper()
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
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestWorkspaceEdgeSetOnPostgres(t *testing.T) {
	database := openIsolated(t)
	ctx := context.Background()
	stamp := fmt.Sprint(time.Now().UnixNano())
	account := "edgeset_account_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	node := func(name, domain string) string {
		id := "edgeset_" + name + "_" + stamp
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region',$2,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,'sha256:' || repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id, domain)
		t.Cleanup(func() {
			_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
		return id
	}
	// Isolate from other fixtures: only this test's nodes may be ready edges.
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE id NOT LIKE $1 AND roles @> ARRAY['edge']`, "edgeset_%"+stamp)
	a, b, c := node("a", "dom-a"), node("b", "dom-b"), node("c", "dom-c")
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	scope := Scope{Kind: "account", ID: account, Account: account}
	reconcile := func(at time.Time) bool {
		t.Helper()
		var changed bool
		err := database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var inner error
			changed, inner = ReconcileScope(ctx, tx, scope, at)
			return inner
		})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	stored := func() (mode string, ids []string, generation int64) {
		t.Helper()
		if err := database.Pool().QueryRow(ctx, `SELECT mode,node_ids,generation FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, account).Scan(&mode, &ids, &generation); err != nil {
			t.Fatal(err)
		}
		return
	}
	allows := func(node string) bool {
		t.Helper()
		var ok bool
		if err := database.Pool().QueryRow(ctx, `SELECT paperboat.workspace_edge_set_allows($1,$2,NULL)`, node, account).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// No set yet: placement is unrestricted.
	if !allows(a) || !allows(b) || !allows(c) {
		t.Fatal("a workspace without a set must not restrict placement")
	}
	// First use: two managed edges in distinct failure domains.
	if !reconcile(time.Now()) {
		t.Fatal("first reconcile stored no set")
	}
	mode, ids, generation := stored()
	if mode != "managed-only" || len(ids) != 2 || generation != 1 || ids[0] == ids[1] {
		t.Fatalf("managed set = %s %v generation %d", mode, ids, generation)
	}
	members := map[string]bool{ids[0]: true, ids[1]: true}
	for _, id := range []string{a, b, c} {
		if allows(id) != members[id] {
			t.Fatalf("set_allows(%s) = %v with members %v", id, allows(id), ids)
		}
	}
	if reconcile(time.Now()) {
		t.Fatal("an unchanged healthy set was rewritten")
	}
	// A member silent for less than the ten-minute replacement window keeps its place...
	lost := ids[0]
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '5 minutes' WHERE id=$1`, lost)
	if reconcile(time.Now()) {
		t.Fatal("a member that missed heartbeats for five minutes was replaced")
	}
	// ...but a member gone for longer is replaced and the survivor keeps its place.
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '20 minutes' WHERE id=$1`, lost)
	if !reconcile(time.Now()) {
		t.Fatal("a member gone for twenty minutes was not replaced")
	}
	_, replaced, generation := stored()
	if generation != 2 || len(replaced) != 2 || replaced[0] != ids[1] || replaced[1] == lost {
		t.Fatalf("after losing %s: %v generation %d (survivor %s)", lost, replaced, generation, ids[1])
	}
	// Switching to the workspace's own servers starts over with exactly those servers: one
	// installation gives a single-edge set, and managed edges are no longer allowed.
	own := node("own", "dom-own")
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'),'fixture','own server','tunnel','account',$3,$1,true)`, "edgeset_inst_"+stamp, own, account)
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2]::text[])`, account, "edgeset_inst_"+stamp)
	if !reconcile(time.Now()) {
		t.Fatal("switching to self-hosted stored no new set")
	}
	mode, ids, generation = stored()
	if mode != "self-hosted-only" || len(ids) != 1 || ids[0] != own || generation != 3 {
		t.Fatalf("self-hosted set = %s %v generation %d", mode, ids, generation)
	}
	if !allows(own) || allows(a) || allows(b) || allows(c) {
		t.Fatal("a self-hosted workspace must only use its own servers")
	}
	// And back to the managed pool.
	exec(`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, account)
	if !reconcile(time.Now()) {
		t.Fatal("switching back to managed stored no new set")
	}
	mode, ids, _ = stored()
	if mode != "managed-only" || len(ids) != 2 || allows(own) {
		t.Fatalf("managed set after switching back = %s %v", mode, ids)
	}
}
