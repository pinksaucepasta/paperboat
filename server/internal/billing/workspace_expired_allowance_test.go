package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestExpiredPersonalSubscriptionDoesNotRenewPaidAllowance(t *testing.T) {
	for _, currentMonth := range []bool{false, true} {
		t.Run(fmt.Sprint("expired-this-month=", currentMonth), func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			suffix := fmt.Sprint(time.Now().UnixNano())
			user, product := "usr_expired_allowance_"+suffix, "expired-allowance-"+suffix
			workspace := "personal:" + user
			insertUser(t, store, user, user+"@example.test")
			t.Cleanup(func() {
				for _, q := range []string{
					`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`,
					`DELETE FROM paperboat.billing_workspaces WHERE id=$1`,
				} {
					if _, err := store.SQL().ExecContext(ctx, q, workspace); err != nil {
						t.Error(err)
					}
				}
				if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_products WHERE code=$1`, product); err != nil {
					t.Error(err)
				}
				if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user); err != nil {
					t.Error(err)
				}
			})
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,$1,'personal_subscription','{"allowance_bytes":100}',true)`, product)
			at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,product_code,state,period_start,period_end) VALUES($1,$2,$3,'active',$4,$5)`, workspace, user, product, at.Add(-time.Hour), at.Add(time.Hour))
			service := billing.NewWorkspaceService(store, nil, "")
			check := func(want *int64) {
				t.Helper()
				err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
					_, err := service.CheckWorkspaceBandwidthTx(ctx, tx, workspace, at)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				var included *int64
				if err := store.SQL().QueryRowContext(ctx, `SELECT included_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2`, workspace, at).Scan(&included); err != nil {
					t.Fatal(err)
				}
				if (included == nil) != (want == nil) || included != nil && *included != *want {
					t.Fatal("new period inherited an allowance from an ineligible subscription")
				}
			}
			paid := int64(100)
			check(&paid)
			// Keep the retained historical period and catalog after the subscription ends.
			// No old-period usage may grant a fresh calendar month's paid allowance.
			expiredStart, expiredEnd := at.AddDate(0, -2, 0), at.AddDate(0, -1, 0)
			if currentMonth {
				expiredEnd = at.Add(-time.Hour)
				expiredStart = expiredEnd.AddDate(0, -1, 0)
			}
			exec(`UPDATE paperboat.billing_workspace_usage_periods SET period_start=$2,period_end=$3 WHERE workspace_id=$1`, workspace, expiredStart, expiredEnd)
			exec(`UPDATE paperboat.billing_workspaces SET period_start=$2,period_end=$3 WHERE id=$1`, workspace, expiredStart, expiredEnd)
			var free *int64
			if err := store.SQL().QueryRowContext(ctx, `SELECT personal_allowance_bytes FROM paperboat.billing_operator_policy WHERE singleton`).Scan(&free); err != nil {
				t.Fatal(err)
			}
			check(free)
			var retained int64
			if err := store.SQL().QueryRowContext(ctx, `SELECT included_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_end=$2`, workspace, expiredEnd).Scan(&retained); err != nil || retained != paid {
				t.Fatal("historical paid allowance changed", err)
			}

			if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				_, err := service.CheckWorkspaceBandwidthTx(ctx, tx, workspace, expiredEnd.Add(-time.Minute))
				return err
			}); err != nil {
				t.Fatal("return to Free invalidated delayed paid reports:", err)
			}

		})
	}
}
