package billing_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// F21-168: an edge's ingress pull re-admits every route it serves. One transaction for the whole group of a node gives each
// binding the same row Record gives it alone, answers a binding that cannot be admitted by itself, and costs one
// transaction, not one per route (1,000 tunnels took 10 s, longer than the 10 s the decisions lived).
func TestRecordManyAdmitsAGroupLikeRecordDoesOneByOne(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user := "usr_many_" + x
	workspace := "personal:" + user
	insertUser(t, store, user, user+"@example.test")
	nodes := [2]string{"edge_many_a_" + x, "edge_many_b_" + x}
	for _, node := range nodes {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.usage_accounting_authorities WHERE workspace_id=$1`, workspace)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
		for _, node := range nodes {
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	policy := billing.NewWorkspaceService(store, nil, "")
	now := time.Now().UTC()
	binding := func(node string, i int, expires time.Time) accountingauthority.Binding {
		return accountingauthority.Binding{ID: fmt.Sprintf("public:pub_%d_%s:1", i, x), Node: node, Environment: "env_" + x, Route: fmt.Sprintf("route_%d_%s", i, x), Revision: 3, User: user, IssuedAt: now, ExpiresAt: expires}
	}
	const routes = 120
	group := make([]accountingauthority.Binding, routes)
	for i := range group {
		group[i] = binding(nodes[0], i, now.Add(10*time.Second))
	}
	group[40].Route = "" // one binding that cannot be admitted answers for itself only
	before := store.TransactionStats()
	results := accountingauthority.RecordMany(ctx, store, group, policy)
	after := store.TransactionStats()
	if got := after.ReadCommitted.Calls - before.ReadCommitted.Calls; got != 1 {
		t.Errorf("a group of %d bindings of one node took %d transactions, want 1", routes, got)
	}
	for i, err := range results {
		if i == 40 {
			if !errors.Is(err, accountingauthority.ErrUnavailable) {
				t.Fatalf("invalid binding answered %v, want ErrUnavailable", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("binding %d: %v", i, err)
		}
	}
	// The same bindings one by one on the other node give the same rows.
	for i := range group {
		if i == 40 {
			continue
		}
		if err := accountingauthority.Record(ctx, store, binding(nodes[1], i, now.Add(10*time.Second)), policy); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(node string) string {
		var out string
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*)||'/'||count(DISTINCT workspace_id)||'/'||min(workspace_id)||'/'||min(actor_account_id)||'/'||min(route_revision)||'/'||(max(admission_expires_at)-min(admitted_at))::text FROM paperboat.usage_accounting_authorities WHERE edge_node_id=$1`, node).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if a, b := rows(nodes[0]), rows(nodes[1]); a != b || a == "" {
		t.Fatalf("group rows %q differ from one-by-one rows %q", a, b)
	}
	// A later pull extends the admission and never shortens it; a stale one leaves the later expiry.
	group[40].Route = "route_40_" + x
	later := now.Add(time.Hour)
	for i := range group {
		group[i].ExpiresAt = later
	}
	for i, err := range accountingauthority.RecordMany(ctx, store, group, policy) {
		if err != nil {
			t.Fatalf("renewal %d: %v", i, err)
		}
	}
	stale := append([]accountingauthority.Binding(nil), group[:5]...)
	for i := range stale {
		stale[i].ExpiresAt = now.Add(time.Second)
	}
	for _, err := range accountingauthority.RecordMany(ctx, store, stale, policy) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var held int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.usage_accounting_authorities WHERE edge_node_id=$1 AND admission_expires_at>=$2`, nodes[0], later.Add(-time.Second)).Scan(&held); err != nil || held != routes {
		t.Fatalf("%d of %d bindings keep the later admission (err %v)", held, routes, err)
	}
	// The same binding twice in one group is one row.
	twice := []accountingauthority.Binding{binding(nodes[0], 1, later), binding(nodes[0], 1, later)}
	for _, err := range accountingauthority.RecordMany(ctx, store, twice, policy) {
		if err != nil {
			t.Fatalf("duplicate binding: %v", err)
		}
	}
}

// exhausted judges every workspace past its fair-use bandwidth.
type exhausted struct{}

func (exhausted) CheckWorkspaceBandwidthTx(context.Context, *db.Tx, string, time.Time) (bool, error) {
	return true, nil
}

// A workspace past its fair-use bandwidth is slowed on Paperboat's own (hosted) edges and never on a customer-owned one:
// the same binding is answered ErrThrottled by the one and admitted plainly by the other.
func TestRecordManySlowsHostedNodesAndNeverCustomerOwnedOnes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user := "usr_slow_" + x
	workspace := "personal:" + user
	insertUser(t, store, user, user+"@example.test")
	hosted, global, own := "edge_slow_hosted_"+x, "edge_slow_global_"+x, "edge_slow_own_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.usage_accounting_authorities WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspaces WHERE id=$1`,
		} {
			_, _ = store.SQL().ExecContext(bg, q, workspace)
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.selfhost_installations WHERE node_id = ANY($1)`, []string{global, own})
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id = ANY($1)`, []string{hosted, global, own})
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	for _, node := range []string{hosted, global, own} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node)
	}
	for _, i := range []struct{ node, scope, scopeID string }{{global, "global", "operator"}, {own, "account", user}} {
		exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete)
 VALUES($1,$1,$2,$2,'fixture','slowdown fixture','tunnel',$3,$4,$1,true)`, i.node, []byte(strings.Repeat("k", 32)), i.scope, i.scopeID)
	}
	now := time.Now().UTC()
	for node, wantThrottled := range map[string]bool{hosted: true, global: true, own: false} {
		b := accountingauthority.Binding{ID: "public:pub_" + node + ":1", Node: node, Environment: "env_" + x, Route: "route_" + node, Revision: 1, User: user, IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}
		err := accountingauthority.Record(ctx, store, b, exhausted{})
		if throttled := errors.Is(err, accountingauthority.ErrThrottled); throttled != wantThrottled || (!throttled && err != nil) {
			t.Errorf("node %s: err=%v, want throttled=%v", node, err, wantThrottled)
		}
	}
}
