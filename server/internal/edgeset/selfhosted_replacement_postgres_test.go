package edgeset

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// A workspace on its own servers keeps two of the servers it selected; one silent for over ten
// minutes is replaced by another selected server, never by a managed edge, and with no other
// selected server the survivor serves alone until the lost one returns.
func TestSelfHostedEdgeSetReplacesOnlyWithSelectedServersOnPostgres(t *testing.T) {
	database := openIsolated(t)
	ctx := context.Background()
	stamp := fmt.Sprint(time.Now().UnixNano())
	account := "edgeset_sh_account_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	var installations []string
	node := func(name, domain string, own bool) string {
		id := "edgeset_sh_" + name + "_" + stamp
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region',$2,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,'sha256:' || repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id, domain)
		t.Cleanup(func() {
			_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
		if own {
			installation := "edgeset_sh_inst_" + name + "_" + stamp
			exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,decode(md5($1||'k')||md5($1||'kk'),'hex'),decode(md5($1||'h')||md5($1||'hh'),'hex'),'fixture',$3,'tunnel','account',$4,$1,true)`, installation, id, name, account)
			installations = append(installations, installation)
		}
		return id
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE id NOT LIKE $1 AND roles @> ARRAY['edge']`, "edgeset_sh_%"+stamp)
	managed := node("managed", "dom-m", false)
	one, two, three := node("one", "dom-1", true), node("two", "dom-2", true), node("three", "dom-3", true)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	// The workspace selected two of its three servers.
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2,$3]::text[])`, account, installations[0], installations[1])
	scope := Scope{Kind: "account", ID: account, Account: account}
	reconcile := func() bool {
		t.Helper()
		var changed bool
		err := database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var inner error
			changed, inner = ReconcileScope(ctx, tx, scope, time.Now())
			return inner
		})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	stored := func() (mode string, ids []string) {
		t.Helper()
		if err := database.Pool().QueryRow(ctx, `SELECT mode,node_ids FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, account).Scan(&mode, &ids); err != nil {
			t.Fatal(err)
		}
		return
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		have := map[string]bool{}
		for _, id := range got {
			have[id] = true
		}
		for _, id := range want {
			if !have[id] {
				return false
			}
		}
		return true
	}
	reconcile()
	if mode, ids := stored(); mode != "self-hosted-only" || !same(ids, one, two) {
		t.Fatalf("selected servers = %s %v, want %s and %s", mode, ids, one, two)
	}
	// Lost for over ten minutes with no other selected server: the survivor serves alone.
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '20 minutes' WHERE id=$1`, one)
	reconcile()
	if _, ids := stored(); !same(ids, two) {
		t.Fatalf("after losing %s with no spare: %v, want only %s (never a managed edge or an unselected server)", one, ids, two)
	}
	// The workspace selects its third server: it replaces the lost one.
	exec(`UPDATE paperboat.selfhost_pool_policies SET installation_ids=ARRAY[$2,$3,$4]::text[] WHERE scope_id=$1`, account, installations[0], installations[1], installations[2])
	// A spare whose public ingress is not verified is kept out of the set until it is.
	exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=NULL WHERE id=$1`, three)
	reconcile()
	if _, ids := stored(); !same(ids, two) {
		t.Fatalf("an unverified spare entered the set: %v", ids)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now() WHERE id=$1`, three)
	reconcile()
	if _, ids := stored(); !same(ids, two, three) {
		t.Fatalf("after selecting a spare: %v, want %s and %s", ids, two, three)
	}
	// The lost server comes back; the healthy pair is left alone.
	exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now() WHERE id=$1`, one)
	if reconcile() {
		t.Fatal("a healthy set was rewritten when a former member returned")
	}
	if mode, ids := stored(); mode != "self-hosted-only" || !same(ids, two, three) {
		t.Fatalf("set after the return = %s %v", mode, ids)
	}
	for _, id := range []string{one, two, three} {
		var ok bool
		if err := database.Pool().QueryRow(ctx, `SELECT paperboat.workspace_edge_set_allows($1,$2,NULL)`, id, account).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if want := id != one; ok != want {
			t.Fatalf("set_allows(%s)=%v, want %v", id, ok, want)
		}
	}
	var ok bool
	if err := database.Pool().QueryRow(ctx, `SELECT paperboat.workspace_edge_set_allows($1,$2,NULL)`, managed, account).Scan(&ok); err != nil || ok {
		t.Fatalf("a self-hosted workspace may use a managed edge: %v %v", ok, err)
	}
}
