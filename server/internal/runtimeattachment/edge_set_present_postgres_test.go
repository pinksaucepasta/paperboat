package runtimeattachment

import (
	"context"
	"testing"
	"time"
)

// An attach of a workspace whose edge set exists neither takes the builder lock nor opens a transaction: the
// attaches of one account must not queue behind each other for a set that is already there.
func TestEnsureEdgeSetSkipsTheBuilderLockWhenTheSetExistsPostgres(t *testing.T) {
	f := newAgentFixture(t, "edgesetpresent")
	f.exec(t, `INSERT INTO workspace_edge_sets(scope_kind,scope_id,mode,node_ids) VALUES('account',$1,'managed-only',ARRAY[$2]::text[])`, f.account, f.node)
	t.Cleanup(func() { f.database.SQL().ExecContext(context.Background(), `DELETE FROM workspace_edge_sets WHERE scope_id=$1`, f.account) })
	unlock, err := edgeSetBuilders.Lock(f.ctx, "account:"+f.account)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	before := f.database.TransactionStats()
	if err := f.service.ensureEdgeSet(ctx, f.proof); err != nil {
		t.Fatalf("an existing edge set waited for the builder lock: %v", err)
	}
	if calls := f.database.TransactionStats().Serializable.Calls - before.Serializable.Calls; calls != 0 {
		t.Fatalf("an existing edge set opened %d serializable transactions", calls)
	}
}
