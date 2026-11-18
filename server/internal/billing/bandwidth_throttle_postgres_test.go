package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// A workspace is throttled (never refused) from the debit that uses its fair-use amount until its allowance
// period ends; an operator's throttle lift and a bandwidth grant end it at once; the next period starts at full
// speed and the bell says so. Notices: 80%, throttled, reset, each once.
func TestBandwidthThrottleTransitionLiftAndResetPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, product := "throttle_u_"+x, "throttle_plan_"+x
	insertUser(t, store, user, user+"@example.test")
	workspace := "personal:" + user
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.announcements WHERE created_by='system' AND idempotency_key LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.audit_events WHERE idempotency_key LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.billing_workspaces WHERE id LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.billing_products WHERE code LIKE '%'||$1||'%'`,
			`DELETE FROM paperboat.users WHERE id LIKE '%'||$1||'%'`,
		} {
			_, _ = store.SQL().ExecContext(context.Background(), q, x)
		}
	})
	at := time.Now().UTC().Truncate(time.Second)
	start, end := at.Add(-time.Hour), at.AddDate(0, 3, 0)
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref) VALUES($1,$1,'stripe',$1,$1,'personal_subscription','{"allowance_bytes":100}')`, product)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state,product_code,period_start,period_end,bandwidth_anchor) VALUES($1,$2,'active',$3,$4,$5,$4)`, workspace, user, product, start, end)

	s := billing.NewWorkspaceService(store, nil, "")
	s.SetThrottleRate(125_000)
	debit := func(n int64, when time.Time) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, _, err := s.DebitWorkspaceBandwidthTx(ctx, tx, workspace, n, when)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	throttled := func(when time.Time) bool {
		t.Helper()
		var out bool
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var err error
			out, err = s.CheckWorkspaceBandwidthTx(ctx, tx, workspace, when)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	titles := func() map[string]int {
		t.Helper()
		view, err := announcements.New(store).ForUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, n := range view.Notices {
			out[n.Title]++
		}
		return out
	}
	const eighty, slowed, back = "80% of fair-use traffic used", "Public traffic slowed to 1 Mbit/s", "Full speed is back"

	debit(79, at)
	if throttled(at) || len(titles()) != 0 {
		t.Fatalf("79 of 100 bytes: throttled=%v notices=%v", throttled(at), titles())
	}
	debit(1, at)
	if throttled(at) || titles()[eighty] != 1 {
		t.Fatalf("80 of 100 bytes: throttled=%v notices=%v, want the 80%% notice and no throttle", throttled(at), titles())
	}
	debit(20, at)
	debit(5, at) // beyond the amount: observed and recorded, no longer admitted at full speed
	if !throttled(at) || titles()[slowed] != 1 || titles()[eighty] != 1 {
		t.Fatalf("100 of 100 bytes: throttled=%v notices=%v, want one throttled notice", throttled(at), titles())
	}
	overview, err := s.Overview(ctx, user, "")
	if err != nil || !overview.Throttled || overview.ThrottleLifted || overview.AllowanceBytes == nil || *overview.AllowanceBytes != 100 || overview.UsageBytes != 105 {
		t.Fatalf("overview = %+v (%v), want throttled, 100 allowance, 105 observed", overview, err)
	}

	// An operator's lift ends the throttle while it runs; revoking it throttles again. It is audited by the operator action.
	var grant planlimits.Grant
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		grant, err = planlimits.GrantTx(ctx, tx, "operator_x", workspace, planlimits.ThrottleLift, 1, nil, "support case")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if throttled(at) {
		t.Fatal("a lifted throttle still throttles")
	}
	if overview, err = s.Overview(ctx, user, ""); err != nil || overview.Throttled || !overview.ThrottleLifted {
		t.Fatalf("overview with a lift = %+v (%v)", overview, err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := planlimits.RevokeTx(ctx, tx, "operator_x", workspace, grant.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !throttled(at) {
		t.Fatal("a revoked lift left the workspace unthrottled")
	}
	// Extra bandwidth an operator grants raises the fair-use amount before throttling.
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := planlimits.GrantTx(ctx, tx, "operator_x", workspace, planlimits.Bandwidth, 50, nil, "extra")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if throttled(at) {
		t.Fatal("a bandwidth grant did not raise the fair-use amount")
	}

	// The next allowance period starts at full speed and says so once.
	next := at.AddDate(0, 1, 7)
	if throttled(next) || titles()[back] != 1 {
		t.Fatalf("next period: throttled=%v notices=%v, want full speed and one reset notice", throttled(next), titles())
	}
	throttled(next)
	if titles()[back] != 1 {
		t.Fatalf("the reset notice repeated: %v", titles())
	}
}
