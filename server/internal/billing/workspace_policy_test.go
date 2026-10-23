package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestUnsetWorkspaceAllowancesAccountUsageWithoutLimitingFree(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "usr_free_usage_" + suffix
	workspace := "personal:" + user
	insertUser(t, store, user, "free-"+suffix+"@example.test")
	t.Cleanup(func() {
		if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE user_id=$1`, user); err != nil {
			t.Error(err)
		}
		for _, query := range []string{`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`} {
			if _, err := store.SQL().ExecContext(context.Background(), query, workspace); err != nil {
				t.Error(err)
			}
		}
		if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user); err != nil {
			t.Error(err)
		}
	})
	service := billing.NewWorkspaceService(store, nil, "")
	if err := service.InitializeAllowancePolicy(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	var originalPersonal, originalTeam sql.NullInt64
	var originalRevision int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT personal_allowance_bytes,team_allowance_bytes,revision FROM paperboat.billing_operator_policy WHERE singleton`).Scan(&originalPersonal, &originalTeam, &originalRevision); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.SQL().ExecContext(context.Background(), `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=$1,team_allowance_bytes=$2,revision=$3,updated_at=now() WHERE singleton`, originalPersonal, originalTeam, originalRevision); err != nil {
			t.Error(err)
		}
	})
	setPolicy := func(personal, team *int64) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=$1,team_allowance_bytes=$2,revision=revision+1,updated_at=now() WHERE singleton`, personal, team); err != nil {
			t.Fatal(err)
		}
	}
	zeroBytes := int64(0)
	setPolicy(&zeroBytes, nil)
	err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		w, err := billing.WorkspaceTx(ctx, tx, user, "", false)
		if err != nil {
			return err
		}
		exhausted, err := service.CheckWorkspaceBandwidthTx(ctx, tx, w.ID, time.Now().UTC())
		if err != nil {
			return err
		}
		if !exhausted {
			t.Error("configured zero admitted first request")
		}
		return errRollback // admission denial rolls back period creation
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	setPolicy(nil, nil)
	if _, err := service.Overview(ctx, user, ""); err != nil {
		t.Fatal(err)
	}
	const used = int64(1 << 40)
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := service.ReserveUserMachineSeat(ctx, tx, user); err != nil {
			return err
		}
		granted, exhausted, err := service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, used, time.Now().UTC())
		if err == nil && (granted != used || exhausted) {
			t.Errorf("unset allowance restricted Free: granted=%d exhausted=%v", granted, exhausted)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	overview, err := service.Overview(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	if overview.AllowanceBytes != nil || overview.UsageBytes != used {
		t.Fatalf("unset usage: allowance=%v usage=%d", overview.AllowanceBytes, overview.UsageBytes)
	}
	// Concurrent enrollments cannot each claim the final configured place.
	// The user already holds one machine from the reservation above; a Free limit of 2 leaves
	// exactly one place for the two racing enrollments.
	limits := config.Default().Billing.Limits
	limits.Machines.Free = 2
	planlimits.Configure(limits, "")
	t.Cleanup(func() { planlimits.Configure(config.Default().Billing.Limits, "") })
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'quota-held','linux','amd64','/workspace','offline','occupied')`, "quota_held_"+suffix, user); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			results <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
				if err := service.ReserveUserMachineSeat(ctx, tx, user); err != nil {
					return err
				}
				id := fmt.Sprintf("quota_%s_%d", suffix, i)
				_, err := tx.Exec(ctx, `INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,$3,'linux','amd64','/workspace','offline','occupied')`, id, user, fmt.Sprintf("quota-%d", i))
				return err
			})
		}(i)
	}
	accepted, denied := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			accepted++
		} else if errors.Is(err, planlimits.ErrLimitReached) {
			denied++
		} else {
			t.Errorf("concurrent enrollment: %v", err)
		}
	}
	if accepted != 1 || denied != 1 {
		t.Fatalf("concurrent quota accepted=%d denied=%d", accepted, denied)
	}
}

var errRollback = errors.New("roll back the test transaction")
