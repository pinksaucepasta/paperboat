package billing_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestWorkspaceUsagePreservesHistoricalPeriodAndOtherPayer(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "usr_usage_scope_" + suffix
	team := "usage-team-" + suffix
	personal := "personal:" + user
	workspace := "team:" + team
	insertUser(t, store, user, "usage-"+suffix+"@example.test")
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id IN ($1,$2)`, `DELETE FROM paperboat.billing_workspaces WHERE id IN ($1,$2)`} {
			if _, err := store.SQL().ExecContext(context.Background(), q, personal, workspace); err != nil {
				t.Error(err)
			}
		}
		if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, team); err != nil {
			t.Error(err)
		}
		if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user); err != nil {
			t.Error(err)
		}
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,user_id,team_id,state) VALUES($1,$2,NULL,'active'),($3,NULL,$4,'canceled')`, personal, user, workspace, team); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-48 * time.Hour)
	end := start.Add(24 * time.Hour)
	for _, id := range []string{personal, workspace} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes) VALUES($1,$2,$3,100)`, id, start, end); err != nil {
			t.Fatal(err)
		}
	}
	service := billing.NewWorkspaceService(store, nil, "")
	for i, want := range []int64{60, 40} {
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			granted, exhausted, err := service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, 60, start.Add(time.Hour))
			if err == nil && (granted != want || exhausted != (i == 1)) {
				t.Errorf("grant=%d exhausted=%v", granted, exhausted)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var own, total int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT consumed_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, personal).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT consumed_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if own != 0 || total != 100 {
		t.Fatalf("personal=%d team=%d", own, total)
	}
}

func TestPaidPeriodReplacesOnlyOverlappingFreeWindow(t *testing.T) {
	for _, interval := range []string{"month", "year"} {
		t.Run(interval, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			suffix := fmt.Sprint(time.Now().UnixNano())
			user, code := "usr_upgrade_"+suffix, "upgrade-"+suffix
			workspace := "personal:" + user
			insertUser(t, store, user, user+"@example.test")
			now := time.Now().UTC().Truncate(time.Second)
			oldStart, oldEnd := now.Add(-24*time.Hour), now.Add(24*time.Hour)
			paidStart := now.Add(-time.Hour)
			paidEnd := paidStart.AddDate(0, 1, 0)
			if interval == "year" {
				paidEnd = paidStart.AddDate(1, 0, 0)
			}
			t.Cleanup(func() {
				for _, q := range []string{`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`} {
					if _, err := store.SQL().ExecContext(context.Background(), q, workspace); err != nil {
						t.Error(err)
					}
				}
				if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code=$1`, code); err != nil {
					t.Error(err)
				}
				if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user); err != nil {
					t.Error(err)
				}
			})
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_interval) VALUES($1,$2,'stripe',$3,$4,'personal_subscription','{"allowance_bytes":100}',true,$5)`, "bp_"+suffix, code, "prod_"+suffix, "price_"+suffix, interval); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,user_id,state) VALUES($1,$2,'active')`, workspace, user); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,observed_bytes) VALUES($1,$2,$3,10,7,7)`, workspace, oldStart, oldEnd); err != nil {
				t.Fatal(err)
			}
			service := billing.NewWorkspaceService(store, nil, "")
			before, err := service.Overview(ctx, user, "")
			if err != nil || before.AllowanceBytes == nil || *before.AllowanceBytes != 10 || before.UsageBytes != 7 {
				t.Fatal("initial Free period is not visible")
			}
			// The verified provider snapshot starts a paid period inside the Free month.
			if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET product_code=$2,period_start=$3,period_end=$4 WHERE id=$1`, workspace, code, paidStart, paidEnd); err != nil {
				t.Fatal(err)
			}
			upgraded, err := service.Overview(ctx, user, "")
			if err != nil || upgraded.AllowanceBytes == nil || *upgraded.AllowanceBytes != 100 || upgraded.UsageBytes != 0 {
				t.Fatal("paid allowance is masked by the existing Free period")
			}
			var paidWindowEnd time.Time
			if err := store.SQL().QueryRowContext(ctx, `SELECT period_end FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start=$2`, workspace, paidStart).Scan(&paidWindowEnd); err != nil {
				t.Fatal(err)
			}
			if !paidWindowEnd.Equal(paidStart.AddDate(0, 1, 0)) {
				t.Fatal("paid bandwidth allowance spans more than one month")
			}
			// Delayed Free reports still belong to its closed window, while new traffic
			// debits the actual provider period without spending its predecessor twice.
			for _, input := range []struct {
				at          time.Time
				bytes, want int64
			}{{paidStart.Add(-time.Minute), 5, 3}, {now, 11, 11}} {
				if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
					granted, _, err := service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, input.bytes, input.at)
					if err == nil && granted != input.want {
						t.Errorf("granted=%d want=%d", granted, input.want)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			var end time.Time
			var included, consumed, observed int64
			if err := store.SQL().QueryRowContext(ctx, `SELECT period_end,included_bytes,consumed_bytes,observed_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start=$2`, workspace, oldStart).Scan(&end, &included, &consumed, &observed); err != nil {
				t.Fatal(err)
			}
			if !end.Equal(paidStart) || included != 10 || consumed != 10 || observed != 12 {
				t.Fatal("closed Free window lost its historical accounting")
			}
			final, err := service.Overview(ctx, user, "")
			if err != nil || final.AllowanceBytes == nil || *final.AllowanceBytes != 100 || final.UsageBytes != 11 {
				t.Fatal("paid period was not stable across repeated reads and delayed reports")
			}

			if interval == "month" {
				assertPriceSwitchBudget(t, store, service, workspace, code, paidStart, now)
			}

			if interval == "year" {
				assertAnnualWindows(t, store, service, workspace, paidStart)
			}

		})
	}
}

func assertAnnualWindows(t *testing.T, store *db.DB, service *billing.WorkspaceService, workspace string, paidStart time.Time) {
	t.Helper()
	ctx := context.Background()
	next := paidStart.AddDate(0, 2, 0)
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		granted, exhausted, err := service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, 100, next.Add(time.Minute))
		if err == nil && (granted != 100 || !exhausted) {
			t.Error("annual subscription did not replenish its monthly allowance")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var start, end time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT period_start,period_end FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2`, workspace, next.Add(time.Minute)).Scan(&start, &end); err != nil {
		t.Fatal(err)
	}
	if !start.Equal(next) || !end.Equal(paidStart.AddDate(0, 3, 0)) {
		t.Fatal("annual allowance lost its monthly anchor")
	}
	// Exercise the real ledger owner at a leap-year, end-of-month anchor.
	parse := func(value string) time.Time {
		t.Helper()
		result, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET period_start=$2,period_end=$3,bandwidth_anchor=$2 WHERE id=$1`, workspace, parse("2028-01-31T12:00:00Z"), parse("2029-01-31T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ at, start, end string }{
		{"2028-02-28T12:00:00Z", "2028-01-31T12:00:00Z", "2028-02-29T12:00:00Z"},
		{"2028-02-29T12:00:00Z", "2028-02-29T12:00:00Z", "2028-03-31T12:00:00Z"},
		{"2028-03-30T12:00:00Z", "2028-02-29T12:00:00Z", "2028-03-31T12:00:00Z"},
		{"2028-03-31T12:00:00Z", "2028-03-31T12:00:00Z", "2028-04-30T12:00:00Z"},
		{"2029-01-30T12:00:00Z", "2028-12-31T12:00:00Z", "2029-01-31T12:00:00Z"},
	} {
		at := parse(tc.at)
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			exhausted, err := service.CheckWorkspaceBandwidthTx(ctx, tx, workspace, at)
			if err == nil && exhausted {
				t.Error("fresh anchored month has no allowance")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var start, end time.Time
		if err := store.SQL().QueryRowContext(ctx, `SELECT period_start,period_end FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2`, workspace, at).Scan(&start, &end); err != nil {
			t.Fatal(err)
		}
		if !start.Equal(parse(tc.start)) || !end.Equal(parse(tc.end)) {
			t.Fatalf("annual monthly window at %s = %s..%s", tc.at, start, end)
		}
	}
}

// Provider price changes can create partial billing periods; those boundaries
// must neither replenish bandwidth nor move the workspace's monthly clock.
func assertPriceSwitchBudget(t *testing.T, store *db.DB, service *billing.WorkspaceService, workspace, code string, anchor, now time.Time) {
	t.Helper()
	ctx := context.Background()
	setPeriod := func(start, end time.Time) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET period_start=$2,period_end=$3 WHERE id=$1`, workspace, start, end); err != nil {
			t.Fatal(err)
		}
	}
	debit := func(at time.Time, n, want int64) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			got, _, err := service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, n, at)
			if err == nil && got != want {
				t.Errorf("price switch granted=%d want=%d at %s", got, want, at)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setPeriod(now.Add(-time.Minute), now.AddDate(1, 0, 0))
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_products SET verified_interval='year' WHERE code=$1`, code); err != nil {
		t.Fatal(err)
	}
	debit(now, 100, 89)
	overview, err := service.Overview(ctx, strings.TrimPrefix(workspace, "personal:"), "")
	if err != nil || overview.UsageBytes != 111 {
		t.Fatal("price switch hid earlier usage", err)
	}
	// Switch back to monthly with a provider renewal BEFORE the original day.
	next := anchor.AddDate(0, 1, 0)
	early := next.Add(-24 * time.Hour)
	setPeriod(now, early)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_products SET verified_interval='month' WHERE code=$1`, code); err != nil {
		t.Fatal(err)
	}
	debit(now.Add(time.Minute), 1, 0)
	setPeriod(early, early.AddDate(0, 1, 0))
	debit(early.Add(time.Minute), 1, 0)
	debit(next, 60, 60)
	// A subsequent partial provider renewal cannot replenish that new month.
	renewal := early.AddDate(0, 1, 0)
	setPeriod(renewal, renewal.AddDate(0, 1, 0))
	debit(renewal.Add(time.Minute), 60, 40)
	// Expiry and repurchase within the original month retain its already spent budget.
	expired := renewal.Add(2 * time.Minute)
	setPeriod(renewal, expired)
	debit(expired.Add(time.Minute), 1, 1)
	resumed := expired.Add(2 * time.Minute)
	setPeriod(resumed, resumed.AddDate(0, 1, 0))
	debit(resumed, 1, 0)
	// Historical traffic still belongs to the closed paid segment.
	debit(expired.Add(-time.Second), 1, 0)
}
