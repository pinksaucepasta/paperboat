package edgeset

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A workspace's first assignment (a GET of the edge pair, or a machine attaching) that read "no set yet" must not
// overwrite a set another transaction stores while it waits: it takes the workspace lock and looks again.
func TestEnsureTxKeepsASetStoredWhileItWaitsOnPostgres(t *testing.T) {
	database := openIsolated(t)
	ctx := context.Background()
	stamp := fmt.Sprint(time.Now().UnixNano())
	account := "edgeset_ensure_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	// Two usable edges, so an assignment that did overwrite would be visible.
	exec(`UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE id NOT LIKE $1 AND roles @> ARRAY['edge']`, "edgeset_ensure_%"+stamp)
	for _, name := range []string{"a", "b"} {
		id := "edgeset_ensure_" + name + "_" + stamp
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region',$1,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,'sha256:' || repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id)
		t.Cleanup(func() {
			_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	scope := Scope{Kind: "account", ID: account, Account: account}
	stored, release, writerDone, ensured := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() { // the user's choice, committed only after the assignment is waiting
		writerDone <- database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			// The key the pool policy writer holds.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('browser-edge-policy:'||$1||':'||$2,0))`, scope.Kind, scope.ID); err != nil {
				return err
			}
			if err := Store(ctx, tx, scope, "managed-only", []string{"chosen_by_user"}, time.Now()); err != nil {
				return err
			}
			close(stored)
			<-release
			return nil
		})
	}()
	<-stored
	go func() {
		ensured <- database.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error { return EnsureTx(ctx, tx, scope, time.Now()) })
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first assignment never waited on the workspace")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-ensured; err != nil {
		t.Fatal(err)
	}
	var ids []string
	var generation int64
	if err := database.Pool().QueryRow(ctx, `SELECT node_ids,generation FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, account).Scan(&ids, &generation); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "chosen_by_user" || generation != 1 {
		t.Fatalf("the first assignment overwrote the chosen set: %v generation %d", ids, generation)
	}
}
