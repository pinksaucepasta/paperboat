package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
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
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user, code := "usr_upgrade_"+suffix, "upgrade-"+suffix
	workspace := "personal:" + user
	insertUser(t, store, user, user+"@example.test")
	now := time.Now().UTC().Truncate(time.Second)
	oldStart, oldEnd := now.Add(-24*time.Hour), now.Add(24*time.Hour)
	paidStart, paidEnd := now.Add(-time.Hour), now.Add(30*24*time.Hour)
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
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'personal_subscription','{"allowance_bytes":100}',true)`, "bp_"+suffix, code, "prod_"+suffix, "price_"+suffix); err != nil {
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
}
