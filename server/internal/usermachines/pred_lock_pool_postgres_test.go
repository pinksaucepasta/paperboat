package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
)

// seedPredPoolAccount seeds one account with 300 machines and two edge latency samples each.
func seedPredPoolAccount(t *testing.T, store *db.DB) (owner, stamp string) {
	t.Helper()
	ctx := t.Context()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	stamp = fmt.Sprint(time.Now().UnixNano())
	owner = "predpool_owner_" + stamp
	seed := func(from, to int, prefix string) {
		exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT $3||'env_'||i||'_'||$4,$3||'env_'||i||'_'||$4,$1 FROM generate_series($2::int,$5::int) i`, owner, from, prefix, stamp, to)
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) SELECT $3||'machine_'||i||'_'||$4,$1,$3||'env_'||i||'_'||$4,$3||'m'||i,'linux','amd64','/workspace','online','occupied',true,1 FROM generate_series($2::int,$5::int) i`, owner, from, prefix, stamp, to)
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner)
	seed(1, 300, "")
	for _, node := range []string{"a", "b"} {
		id := "predpool_" + node + "_" + stamp
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region',$1,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour','edge.example.test',7272,7273,'sha256:' || repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id)
		exec(`INSERT INTO paperboat.machine_node_latency(machine_id,node_id,role,rtt_ms,measured_at) SELECT 'machine_'||i||'_'||$2,$1,'edge',10+i%50,now() FROM generate_series(1,300) i`, id, stamp)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, owner)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE 'predpool\_%' AND id LIKE '%'||$1`, stamp)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE user_id=$1`, owner)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_environments WHERE owner_user_id=$1`, owner)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	return owner, stamp
}

// startPredPoolLoad runs the background load of the pool tests: the edge-set reconcile and new machines enrolling.
func startPredPoolLoad(t *testing.T, store *db.DB, owner, stamp string) (stop func()) {
	t.Helper()
	ctx := t.Context()
	loadCtx, stopLoad := context.WithCancel(ctx)
	var load sync.WaitGroup
	stop = func() { stopLoad(); load.Wait() }
	t.Cleanup(stop)
	load.Add(2)
	go func() {
		defer load.Done()
		for loadCtx.Err() == nil {
			_, _ = edgeset.ReconcileAll(loadCtx, store, time.Now())
		}
	}()
	go func() {
		defer load.Done()
		for i := 1000; loadCtx.Err() == nil; i++ {
			_, _ = store.SQL().ExecContext(loadCtx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, fmt.Sprintf("predpool_enroll_env_%d_%s", i, stamp), owner)
			_, _ = store.SQL().ExecContext(loadCtx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',true,1)`, fmt.Sprintf("predpool_enroll_machine_%d_%s", i, stamp), owner, fmt.Sprintf("predpool_enroll_env_%d_%s", i, stamp), fmt.Sprintf("enroll%d", i))
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return stop
}

// On prod the predicate-lock pool (64 x 100 locks) filled in bursts and a machine revoke ran into its 10 s deadline
// on "out of shared memory ... max_pred_locks_per_transaction" (53200, 2026-11-19). The pool is a restart-only
// setting, so this test needs a PostgreSQL started with a tiny one (see the skip message) and is skipped everywhere
// else. A long serializable reader fills the pool, and a revoke made while it is full must wait it out and commit.
func TestRevokeCompletesWhenThePredicateLockPoolIsSmallOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	perTx := requireSmallPredicateLockPool(t, store)
	owner, stamp := seedPredPoolAccount(t, store)
	stop := startPredPoolLoad(t, store, owner, stamp)

	// The holder: a serializable transaction reading every table until the pool is full, then held open briefly.
	full, released := make(chan error, 1), make(chan error, 1)
	go func() {
		released <- holdPredicateLockPoolFull(ctx, store.SQL(), full, 400*time.Millisecond)
	}()
	// full carries nil only once the holder itself got 53200: a pass always means the pool really filled.
	if err := <-full; err != nil {
		t.Fatalf("the predicate-lock pool was not filled: %v", err)
	}
	t.Log("pool filled: the holder hit SQLSTATE 53200 (out of shared memory, max_pred_locks_per_transaction)")
	var peak int
	count := func() int {
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE mode='SIReadLock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	peak = count()
	start := time.Now()
	err := New(store, nil, Policy{}, nil).Disconnect(ctx, owner, "machine_7_"+stamp)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("revoke while the pool was full (took %v): %v", took, err)
	}
	if took > 2*time.Second {
		t.Fatalf("revoke took %v, want under 2 s", took)
	}
	if err := <-released; err != nil {
		t.Fatalf("holder: %v", err)
	}
	var state string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machines WHERE id=$1`, "machine_7_"+stamp).Scan(&state); err != nil || state != "disconnected" {
		t.Fatalf("revoked machine state = %q (%v), want disconnected", state, err)
	}
	// Predicate locks are not leaked: once nothing overlaps them they all go.
	stop()
	deadline := time.Now().Add(5 * time.Second)
	for left := count(); left > 0; left = count() {
		if time.Now().After(deadline) {
			t.Fatalf("%d predicate locks remain 5 s after every transaction ended (peak %d while full)", left, peak)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("pool of %d locks per transaction: %d SIREAD locks while full, revoke took %v, %+v", perTx, peak, took, store.TransactionStats().Serializable)
}

// holdPredicateLockPoolFull opens a serializable transaction and reads each table of the schema (a sequential scan
// takes a relation-level SIREAD lock) until PostgreSQL answers 53200, which the savepoint absorbs (the locks it took
// stay). It then reports nil on full (and only then: a nil is proof of 53200, an error covers every other end) and
// commits after hold.
func holdPredicateLockPoolFull(ctx context.Context, database *sql.DB, full chan<- error, hold time.Duration) (err error) {
	reported := false
	report := func(e error) {
		if !reported {
			reported = true
			full <- e
		}
	}
	defer func() { report(err) }()
	conn, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='paperboat' ORDER BY tablename`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, name := range tables {
		if _, err = tx.ExecContext(ctx, `SAVEPOINT fill`); err != nil {
			return err
		}
		var n int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM paperboat.`+quoteIdent(name)).Scan(&n)
		if err == nil {
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "53200" || !strings.Contains(pgErr.Hint, "max_pred_locks_per_transaction") {
			return err
		}
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT fill`); err != nil {
			return err
		}
		report(nil)
		time.Sleep(hold)
		return tx.Commit()
	}
	return fmt.Errorf("read all %d tables and the predicate-lock pool never filled (run PostgreSQL with `docker run ... postgres:17 %s`)", len(tables), tinyPoolFlags)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
