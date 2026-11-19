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
	// The sweep lists workspaces that own a machine.
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,enrolled_at,last_seen_at,alias,installation_generation,worker_generation,connector_state,configured_capabilities,observed_capabilities,host_service_version)
VALUES($1,$2,$3,'linux','amd64','/home/t','online','occupied',true,now(),now(),'edgeset',1,1,'ready',ARRAY['terminal_host'],ARRAY['terminal_host'],'2026.11.19.1')`, "edgeset_machine_"+stamp, account, "edgeset_env_"+stamp)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, "edgeset_machine_"+stamp)
	})
	scope := Scope{Kind: "account", ID: account, Account: account}
	reconcile := func(at time.Time) bool {
		t.Helper()
		var changed bool
		err := database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var inner error
			changed, inner = ReconcileScope(ctx, tx, scope, nil, at)
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
	// The worker's sweep lists only workspaces ReconcileScope would change: not this settled one, however long it lasts.
	listed := func() bool {
		t.Helper()
		rows, err := database.Pool().Query(ctx, scopesSQL, "", "", 100000, Size, time.Now(), fmt.Sprintf("%d seconds", int(ReplaceAfter.Seconds())))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var kind, id, owner string
			if err := rows.Scan(&kind, &id, &owner); err != nil {
				t.Fatal(err)
			}
			if kind == "account" && id == account {
				return true
			}
		}
		return false
	}
	if listed() {
		t.Fatal("the sweep lists a settled workspace")
	}
	// A member silent for less than the ten-minute replacement window keeps its place...
	lost := ids[0]
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '5 minutes' WHERE id=$1`, lost)
	if reconcile(time.Now()) {
		t.Fatal("a member that missed heartbeats for five minutes was replaced")
	}
	if listed() {
		t.Fatal("the sweep lists a workspace whose member is silent for less than the replacement window")
	}
	// ...but a member gone for longer is replaced and the survivor keeps its place.
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '20 minutes' WHERE id=$1`, lost)
	if !listed() {
		t.Fatal("the sweep skips a workspace whose member is gone for twenty minutes")
	}
	if !reconcile(time.Now()) {
		t.Fatal("a member gone for twenty minutes was not replaced")
	}
	if listed() {
		t.Fatal("the sweep still lists a workspace after its member was replaced")
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

// nearestFixture is a workspace with one environment, a set of edges in distinct failure domains (all
// other edges made unready) and a way to add machines with an RTT sample per edge.
type nearestFixture struct {
	t       *testing.T
	ctx     context.Context
	db      *db.DB
	stamp   string
	account string
}

func newNearestFixture(t *testing.T) *nearestFixture {
	f := &nearestFixture{t: t, ctx: context.Background(), db: openIsolated(t), stamp: fmt.Sprint(time.Now().UnixNano())}
	f.account = "edgenear_account_" + f.stamp
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE id NOT LIKE $1 AND roles @> ARRAY['edge']`, "edgenear_%"+f.stamp)
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, f.account)
	t.Cleanup(func() {
		_, _ = f.db.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, f.account)
		_, _ = f.db.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, f.account)
		_, _ = f.db.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`, f.account)
		_, _ = f.db.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.users WHERE id=$1`, f.account)
	})
	return f
}

func (f *nearestFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.SQL().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

func (f *nearestFixture) node(name string) string {
	id := "edgenear_" + name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region',$1,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,'sha256:' || repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id)
	f.t.Cleanup(func() {
		_, _ = f.db.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
	})
	return id
}

// machine adds a live machine with one edge sample (rtt in ms, age as an interval) per node.
func (f *nearestFixture) machine(name string, rtt map[string]int, age string) {
	id, env := "edgenear_machine_"+name+"_"+f.stamp, "edgenear_env_"+name+"_"+f.stamp
	f.exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, f.account)
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',true,1)`, id, f.account, env, name)
	for node, ms := range rtt {
		f.exec(`INSERT INTO paperboat.machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) VALUES($1,$2,'edge',$3,now()-$4::interval)`, id, node, ms, age)
	}
}

func (f *nearestFixture) pair() []string {
	f.t.Helper()
	scope := Scope{Kind: "account", ID: f.account, Account: f.account}
	rtt, err := ReadLatency(f.ctx, f.db.Pool(), scope, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	err = f.db.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := ReconcileScope(ctx, tx, scope, rtt, time.Now())
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	if err = f.db.Pool().QueryRow(f.ctx, `SELECT node_ids FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, f.account).Scan(&ids); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

func TestWorkspaceEdgeSetPrefersNearestOnPostgres(t *testing.T) {
	f := newNearestFixture(t)
	slow, mid, near, stale := f.node("slow"), f.node("mid"), f.node("near"), f.node("stale")
	f.machine("one", map[string]int{slow: 300, mid: 40, near: 5}, "1 hour")
	f.machine("old", map[string]int{stale: 1}, "48 hours")
	if ids := f.pair(); len(ids) != 2 || ids[0] != near || ids[1] != mid {
		t.Fatalf("pair = %v, want [%s %s] (the stale 1 ms sample of %s must be ignored)", ids, near, mid, stale)
	}
}

// One machine near edge A and three near edge B: the median, not the single best machine, places the workspace.
func TestWorkspaceEdgeSetRanksByMedianMachineOnPostgres(t *testing.T) {
	f := newNearestFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.machine("m1", map[string]int{a: 5, b: 100, c: 200}, "1 hour")
	for _, name := range []string{"m2", "m3", "m4"} {
		f.machine(name, map[string]int{a: 100, b: 10, c: 200}, "1 hour")
	}
	if ids := f.pair(); len(ids) != 2 || ids[0] != b || ids[1] != a {
		t.Fatalf("pair = %v, want [%s %s]: three machines near B outvote the one near A", ids, b, a)
	}
}

// The latency ranking is read before the serializable reconcile transaction: inside it, its SIREAD locks over
// every machine's samples exhausted Postgres's predicate-lock memory (08:48Z 2026-11-19) and made revokes of
// those machines fail. The reconcile holds no predicate lock on machines or samples, and a revoke of one of
// the scope's machines commits while the reconcile is still open.
func TestReconcileLeavesMachinesAloneAndARevokeCommitsOnPostgres(t *testing.T) {
	f := newNearestFixture(t)
	a, b := f.node("a"), f.node("b")
	for _, name := range []string{"m1", "m2", "m3"} {
		f.machine(name, map[string]int{a: 5, b: 50}, "1 hour")
	}
	scope := Scope{Kind: "account", ID: f.account, Account: f.account}
	rtt, err := ReadLatency(f.ctx, f.db.Pool(), scope, time.Now())
	if err != nil || len(rtt) != 2 {
		t.Fatalf("ReadLatency = %v, %v", rtt, err)
	}
	start := time.Now()
	err = f.db.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := ReconcileScope(ctx, tx, scope, rtt, time.Now()); err != nil {
			return err
		}
		var held int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE mode='SIReadLock' AND pid=pg_backend_pid() AND (relation IN ('paperboat.user_machines'::regclass,'paperboat.machine_node_latency'::regclass) OR relation IN (SELECT indexrelid FROM pg_index WHERE indrelid IN ('paperboat.user_machines'::regclass,'paperboat.machine_node_latency'::regclass)))`).Scan(&held); err != nil {
			return err
		}
		if held != 0 {
			t.Errorf("the reconcile transaction holds %d predicate locks on machines or their latency samples", held)
		}
		// A revoke of one of the scope's machines, while the reconcile is open.
		return f.db.InTx(ctx, func(ctx context.Context, other *db.Tx) error {
			_, err := other.Exec(ctx, `UPDATE paperboat.user_machines SET revoked_at=now(), state='disconnected' WHERE id=$1`, "edgenear_machine_m1_"+f.stamp)
			return err
		})
	})
	if err != nil {
		t.Fatalf("reconcile with a concurrent revoke: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("reconcile and revoke took %v, want under 2 s", took)
	}
}
