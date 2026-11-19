package peersessions

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// One change in a 200-machine account (a machine's endpoints are published) makes every endpoint renew its
// configuration. Each renewal read the whole mesh log of the account (an hour of superseded entries) and decoded
// every peer again: N renewals x N peers per change (prod 11-19: 300-400 ms per build at 88-169 peers, 2,187 slow
// builds in 2 h). The renewals of one change read only the log entries after the mesh they already hold.
func TestOneChangeInATwoHundredMachineAccountCostsItsChangedPeersOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	s := fmt.Sprint(time.Now().UnixNano())
	account := "churn_user_" + s
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	const machines = 200
	seedMeshMachines(t, ctx, store, account, 0, machines-1)
	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
	seq, err := service.refreshMesh(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	// An hour of endpoint publishes as the log keeps them (config.Retention.SupersededMeshEntries): 50 per endpoint.
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_mesh_log(account_id,mesh_seq,endpoint_id,op,entry)
SELECT account_id,$2+row_number() OVER (ORDER BY r,mesh_seq),endpoint_id,'update',entry FROM paperboat.peer_mesh_log, generate_series(1,50) r WHERE account_id=$1`, account, seq); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.peer_mesh_accounts SET mesh_seq=mesh_seq+50*$2 WHERE account_id=$1`, account, machines); err != nil {
		t.Fatal(err)
	}
	type totals struct {
		builds                        uint64
		queries                       int64
		wall, build, sign, generation time.Duration
	}
	var queries atomic.Int64
	store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		queries.Add(1)
		return ctx, func(string) {}
	})
	t.Cleanup(func() { store.SetQueryObserver(nil) })
	round := func(name string) totals {
		t.Helper()
		before, q := service.cache.builds, queries.Load()
		var out totals
		started := time.Now()
		for i := range machines {
			var trace configTrace
			in := NetworkConfigRequest{OperationID: fmt.Sprintf("churn_%s_%s_%d", s, name, i), UserID: account, EndpointID: fmt.Sprintf("%s_m_%d", account, i), Authority: AuthorityUntilRevoked}
			if _, err := service.configuration(ctx, in, &trace); err != nil {
				t.Fatalf("%s %d: %v", name, i, err)
			}
			out.build += trace.build
			out.sign += trace.sign
			out.generation += trace.generation
		}
		out.wall, out.builds, out.queries = time.Since(started), service.cache.builds-before, queries.Load()-q
		t.Logf("%s: %d configurations, %d policy builds, %d queries, wall %v (build %v, sign %v, generation %v), per build %v",
			name, machines, out.builds, out.queries, out.wall, out.build, out.sign, out.generation, out.build/time.Duration(max(out.builds, 1)))
		return out
	}
	round("first")
	warm := round("warm")
	if warm.builds != 0 {
		t.Fatalf("an unchanged account rebuilt %d documents", warm.builds)
	}
	// One machine publishes its endpoints: every peer's document names it, so every endpoint renews.
	var trace configTrace
	if _, err = service.configuration(ctx, NetworkConfigRequest{OperationID: "churn_publish_" + s, UserID: account, EndpointID: account + "_m_0",
		Endpoints: []string{"198.51.100.7:41641"}, Authority: AuthorityUntilRevoked}, &trace); err != nil {
		t.Fatal(err)
	}
	rowsBefore := service.mesh.rows
	changed := round("after one change")
	rowsRead := service.mesh.rows - rowsBefore
	if changed.builds > machines {
		t.Fatalf("one change rebuilt %d documents for %d endpoints", changed.builds, machines)
	}
	perBuild := changed.build / time.Duration(max(changed.builds, 1))
	t.Logf("RESULT one change, %d endpoints: %d builds, %d queries, %d mesh log rows read, %v building, %v per build",
		machines, changed.builds, changed.queries, rowsRead, changed.build, perBuild)
	// The renewals read the changed entry once between them, not the account's log once each (10,200 rows x 200).
	if rowsRead > 2 {
		t.Fatalf("one change read %d mesh log rows, want its changed entry only", rowsRead)
	}
	if perBuild > 50*time.Millisecond {
		t.Fatalf("a renewal after one change took %v to build, want well under 50 ms", perBuild)
	}
	// What the renewals read is exactly the log's view of the account, and it carries the change.
	seq, err = service.refreshMesh(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	held, err := service.mesh.at(ctx, tx, account, seq)
	if err != nil {
		t.Fatal(err)
	}
	logged, err := latestMeshEntries(ctx, tx, account, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != machines || !reflect.DeepEqual(held, logged) {
		t.Fatalf("held mesh (%d entries) differs from the log's (%d)", len(held), len(logged))
	}
	if got := held[account+"_m_0"].Endpoints; !slices.Equal(got, []string{"198.51.100.7:41641"}) {
		t.Fatalf("the changed machine's entry has endpoints %v", got)
	}
}
