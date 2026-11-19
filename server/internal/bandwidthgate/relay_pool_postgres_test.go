package bandwidthgate

import (
	"context"
	"testing"
	"time"
)

// A relay slows an account to its plan's slow speed once ANY workspace it belongs to used its pool (edge and relay traffic share one
// pool per workspace): the personal workspace, or a team it is an active member of. An operator's throttle lift ends it.
func TestRelayPoolSlowsAnAccountWhenAnyWorkspaceIsOutPostgres(t *testing.T) {
	store := gateStore(t)
	f := newGateFixture(t, store)
	ctx := context.Background()
	out := func() bool {
		t.Helper()
		got, err := NewRelayPool(store).lookup(ctx, f.user, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if out() {
		t.Fatal("slowed with no usage at all")
	}
	f.period(f.personal, 100, 60, false)
	f.period(f.teamWS, 100, 40, false)
	if out() {
		t.Fatal("slowed with both pools under their amount")
	}
	// 2 GB relay + 3 GB edge is one 5 GB debit on the workspace: the pool is out, whichever route the bytes took.
	f.exec(`UPDATE paperboat.billing_workspace_usage_periods SET consumed_bytes=100 WHERE workspace_id=$1`, f.personal)
	if !out() {
		t.Fatal("the personal pool is used up and the account was not slowed")
	}
	f.exec(`UPDATE paperboat.billing_workspace_usage_periods SET consumed_bytes=60 WHERE workspace_id=$1`, f.personal)
	f.exec(`UPDATE paperboat.billing_workspace_usage_periods SET consumed_bytes=100 WHERE workspace_id=$1`, f.teamWS)
	if !out() {
		t.Fatal("the team pool is used up and its member was not slowed")
	}
	f.exec(`INSERT INTO paperboat.billing_workspace_grants(id,workspace_id,kind,amount,reason,created_by) VALUES($1,$2,'throttle_lift',1,'test','operator')`, "grant_rp_"+f.user, f.teamWS)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_grants WHERE id=$1`, "grant_rp_"+f.user)
	})
	if out() {
		t.Fatal("a lifted throttle still slowed the account")
	}
}
