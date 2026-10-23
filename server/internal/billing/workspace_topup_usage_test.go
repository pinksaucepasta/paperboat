package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestPaidWorkspaceSpendsPlanBeforeTopupsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, workspace, code, node := "usr_pool_"+x, "pool_"+x, "plan_"+x, "edge_pool_"+x
	insertUser(t, store, user, user+"@example.test")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.usage_accounting_authorities WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_topups WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspaces WHERE id=$1`,
		} {
			exec(q, workspace)
		}
		exec(`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		exec(`DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		exec(`DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	at := time.Now().UTC().Truncate(time.Second)
	start, end := at.Add(-time.Hour), at.AddDate(0, 1, 0)
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref) VALUES($1,$1,'stripe',$1,$1,'personal_subscription','{"allowance_bytes":100}')`, code)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state,product_code,period_start,period_end,bandwidth_anchor) VALUES($1,$2,'active',$3,$4,$5,$4)`, workspace, user, code, start, end)
	exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'topup','fixture','succeeded')`, x, workspace, user)
	exec(`INSERT INTO paperboat.billing_workspace_topups(provider_session_id,operation_key,workspace_id,purchased_bytes,remaining_bytes,purchased_at) VALUES($1,$1,$2,50,50,$3)`, x, workspace, at.Add(-time.Minute))
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node)
	s := billing.NewWorkspaceService(store, nil, "")
	debit := func(n, want int64, exhausted bool) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			got, full, err := s.DebitWorkspaceBandwidthTx(ctx, tx, workspace, n, at)
			if err == nil && (got != want || full != exhausted) {
				t.Errorf("debit %d: granted=%d exhausted=%v, want %d/%v", n, got, full, want, exhausted)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	remaining := func(want int64) {
		t.Helper()
		var got int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT remaining_bytes FROM paperboat.billing_workspace_topups WHERE workspace_id=$1`, workspace).Scan(&got); err != nil || got != want {
			t.Fatalf("topup remaining=%d want=%d: %v", got, want, err)
		}
	}
	debit(100, 100, false)
	remaining(50)
	exec(`UPDATE paperboat.billing_workspaces SET state='canceled' WHERE id=$1`, workspace)
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		full, err := s.CheckWorkspaceBandwidthTx(ctx, tx, workspace, at)
		if err == nil && !full {
			t.Error("unpaid workspace admitted using retained top-up credit")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	remaining(50)
	exec(`UPDATE paperboat.billing_workspaces SET state='active' WHERE id=$1`, workspace)
	// The real admission owner must not re-deny an exhausted plan with credit.
	b := accountingauthority.Binding{ID: "authority_" + x, Node: node, Environment: "env_" + x, Route: "route_" + x, User: user, Revision: 1, IssuedAt: at, ExpiresAt: at.Add(time.Minute)}
	if err := accountingauthority.Record(ctx, store, b, s); err != nil {
		t.Fatalf("paid top-up admission: %v", err)
	}
	rollback := fmt.Errorf("rollback usage receipt")
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, _, err := s.DebitWorkspaceBandwidthTx(ctx, tx, workspace, 30, at); err != nil {
			return err
		}
		return rollback
	}); err != rollback {
		t.Fatal(err)
	}
	remaining(50)
	debit(30, 30, false)
	remaining(20)
	debit(30, 20, true)
	remaining(0)
	var consumed, observed int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT consumed_bytes,observed_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace).Scan(&consumed, &observed); err != nil || consumed != 100 || observed != 160 {
		t.Fatalf("plan accounting=%d/%d: %v", consumed, observed, err)
	}
}
