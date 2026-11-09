package billing_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A top-up belongs to the allowance month it was bought in: it is drawn after the plan allowance, and
// what is unused is gone when the month renews (monthly plan: with the plan renewal on the 11th).
func TestTopupEndsWithAllowanceMonthPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, workspace, code := "usr_period_"+x, "personal:usr_period_"+x, "plan_period_"+x
	insertUser(t, store, user, user+"@example.test")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.billing_workspace_topups WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspaces WHERE id=$1`,
		} {
			exec(q, workspace)
		}
		exec(`DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		exec(`DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	day := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, time.UTC) }
	planStart := time.Date(2025, time.December, 11, 0, 0, 0, 0, time.UTC)
	renewal := day(time.January, 11, 0) // the plan renews on the 11th
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref) VALUES($1,$1,'stripe',$1,$1,'personal_subscription','{"allowance_bytes":100}')`, code)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state,product_code,period_start,period_end,bandwidth_anchor) VALUES($1,$2,'active',$3,$4,$5,$4)`, workspace, user, code, planStart, planStart.AddDate(1, 0, 0))
	// Bought on the 8th and the 9th of the month that renews on the 11th: both end on the 11th.
	for i, p := range []struct {
		bytes int64
		at    time.Time
	}{{30, day(time.January, 8, 10)}, {50, day(time.January, 9, 10)}} {
		key := fmt.Sprintf("%s_%d", x, i)
		exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'topup','fixture','succeeded')`, key, workspace, user)
		exec(`INSERT INTO paperboat.billing_workspace_topups(provider_session_id,operation_key,workspace_id,purchased_bytes,remaining_bytes,purchased_at,expires_at) VALUES($1,$1,$2,$3,$3,$4,$5)`, key, workspace, p.bytes, p.at, renewal)
	}
	s := billing.NewWorkspaceService(store, nil, "")
	debit := func(at time.Time, n, want int64, exhausted bool) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			got, full, err := s.DebitWorkspaceBandwidthTx(ctx, tx, workspace, n, at)
			if err == nil && (got != want || full != exhausted) {
				t.Errorf("debit %d at %s: granted=%d exhausted=%v, want %d/%v", n, at, got, full, want, exhausted)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	remaining := func(at time.Time) (list []billing.WorkspaceTopup, bought, left int64) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			list, bought, left, err = billing.WorkspaceTopupsTx(ctx, tx, workspace, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	spent := func() (a, b int64) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT remaining_bytes FROM paperboat.billing_workspace_topups WHERE workspace_id=$1 AND purchased_bytes=30),(SELECT remaining_bytes FROM paperboat.billing_workspace_topups WHERE workspace_id=$1 AND purchased_bytes=50)`, workspace).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Purchases appear with their own expiry and the month's total extra.
	list, bought, left := remaining(day(time.January, 10, 0))
	if len(list) != 2 || bought != 80 || left != 80 || !list[0].ExpiresAt.Equal(renewal) || !list[1].ExpiresAt.Equal(renewal) {
		t.Fatalf("mid-period overview %+v bought=%d left=%d", list, bought, left)
	}
	if list, bought, _ = remaining(day(time.January, 8, 9)); len(list) != 0 || bought != 0 {
		t.Fatalf("future purchase listed: %+v", list)
	}

	// Draw order: the plan allowance first, then the top-up that was bought first.
	debit(day(time.January, 10, 0), 90, 90, false)
	if a, b := spent(); a != 30 || b != 50 {
		t.Fatalf("top-up drawn before the plan allowance was spent: %d/%d", a, b)
	}
	debit(day(time.January, 10, 1), 30, 30, false) // 10 from the plan, 20 from the first top-up
	if a, b := spent(); a != 10 || b != 50 {
		t.Fatalf("draw order: %d/%d, want 10/50", a, b)
	}
	if _, bought, left = remaining(day(time.January, 10, 2)); bought != 80 || left != 60 {
		t.Fatalf("bought=%d left=%d, want 80/60", bought, left)
	}

	// Renewal: the plan allowance is fresh, the unused 60 bytes are gone and no longer listed.
	if list, bought, left = remaining(renewal); len(list) != 0 || bought != 0 || left != 0 {
		t.Fatalf("expired top-ups still shown: %+v %d %d", list, bought, left)
	}
	debit(renewal, 40, 40, false)
	debit(renewal.Add(time.Hour), 200, 60, true) // only what is left of the new plan month
	if a, b := spent(); a != 10 || b != 50 {
		t.Fatalf("expired top-up was drawn: %d/%d", a, b)
	}
}

// A top-up that predates the expiry (expires_at='infinity' after migration 249) made the overview
// fail with 503 in production; migration 254 dates it, and the overview reads it afterwards.
func TestLegacyTopupExpiryMigrationKeepsOverviewReadablePostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, workspace := "usr_legacy_"+x, "personal:usr_legacy_"+x
	insertUser(t, store, user, user+"@example.test")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		exec(`DELETE FROM paperboat.billing_workspace_topups WHERE workspace_id=$1`, workspace)
		exec(`DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, workspace)
		exec(`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
		exec(`DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
		exec(`DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state) VALUES($1,$2,'free')`, workspace, user)
	exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'topup','fixture','succeeded')`, x, workspace, user)
	exec(`INSERT INTO paperboat.billing_workspace_topups(provider_session_id,operation_key,workspace_id,purchased_bytes,remaining_bytes,purchased_at,expires_at) VALUES($1,$1,$2,50,50,now()-interval '1 day','infinity')`, x, workspace)
	file, err := os.ReadFile("../db/migrations/254_workspace_topup_legacy_expiry.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(file), "-- +goose Down")
	exec(strings.ReplaceAll(up, "-- +goose Up", ""))
	s := billing.NewWorkspaceService(store, nil, "")
	overview, err := s.Overview(ctx, user, "")
	if err != nil {
		t.Fatalf("overview after the legacy top-up was dated: %v", err)
	}
	if overview.TopupBytesRemaining != 50 || len(overview.Topups) != 1 || overview.Topups[0].ExpiresAt.Before(time.Now()) {
		t.Fatalf("legacy top-up %+v remaining=%d", overview.Topups, overview.TopupBytesRemaining)
	}
}
