package billing_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Every edge node admits routes (Record) for the same workspace while other edges debit its usage. They all
// lock the workspace's period row: at serializable they aborted each other (40001, 46-69 per 3 minutes on the
// dev database); now they queue on the row lock, none fails or gives up, and every debited byte is counted.
func TestAdmissionsAndDebitsOfOneWorkspaceAtOnceNeverAbortAndCountExactly(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user := "usr_adm_cc_" + x
	workspace := "personal:" + user
	insertUser(t, store, user, user+"@example.test")
	const nodes, debits, bytes = 6, 10, 7
	nodeID := func(i int) string { return fmt.Sprintf("edge_adm_%d_%s", i, x) }
	for i := range nodes {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, nodeID(i)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.usage_accounting_authorities WHERE workspace_id=$1`, workspace)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
		for i := range nodes {
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, nodeID(i))
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	service := billing.NewWorkspaceService(store, nil, "")
	before := store.TransactionStats()
	at := time.Now().UTC()
	var wg sync.WaitGroup
	errs := make(chan error, nodes*(debits+1))
	for i := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := accountingauthority.Binding{ID: "auth_" + x, Node: nodeID(i), Environment: "env_" + x, Route: "route_" + x, User: user, Revision: 1, IssuedAt: at, ExpiresAt: at.Add(time.Minute)}
			if err := accountingauthority.Record(ctx, store, b, service); err != nil {
				errs <- fmt.Errorf("admit on %s: %w", nodeID(i), err)
			}
			for range debits {
				errs <- store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
					_, _, err := service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, bytes, at)
					return err
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	after := store.TransactionStats()
	if got := after.Serializable.Retries - before.Serializable.Retries + after.ReadCommitted.Retries - before.ReadCommitted.Retries; got != 0 {
		t.Errorf("%d transaction attempts were cancelled and replayed (40001/40P01)", got)
	}
	var admitted, observed int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.usage_accounting_authorities WHERE workspace_id=$1),
 (SELECT coalesce(sum(observed_bytes),0) FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1)`, workspace).Scan(&admitted, &observed); err != nil {
		t.Fatal(err)
	}
	if admitted != nodes || observed != nodes*debits*bytes {
		t.Fatalf("admitted=%d observed=%d, want %d and %d", admitted, observed, nodes, nodes*debits*bytes)
	}
}
