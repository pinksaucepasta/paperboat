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
