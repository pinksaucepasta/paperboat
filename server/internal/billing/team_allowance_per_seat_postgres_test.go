package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// A paid team's included public traffic is the per-seat amount (config billing.fair_use.team_per_seat_bytes) times its seats,
// pooled in the team workspace: the overview says so (seats, per-seat amount, the product), the relay allowance is likewise per seat
// times seats, a seat change mid-period applies at once to every reader of the period row, and the team throttles when the whole
// pool is used.
func TestTeamAllowanceIsPerSeatPooledPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, team, product := "seat_o_"+x, "seat-team-"+x, "seat_plan_"+x
	insertUser(t, store, owner, owner+"@example.test")
	workspace := "team:" + team
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id=$1`,
			`DELETE FROM paperboat.announcements WHERE created_by='system' AND idempotency_key LIKE '%'||$2||'%'`,
			`DELETE FROM paperboat.audit_events WHERE idempotency_key LIKE '%'||$2||'%'`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspaces WHERE id=$1`,
			`DELETE FROM paperboat.team_members WHERE team_id=$3`,
			`DELETE FROM paperboat.teams WHERE team_id=$3`,
			`DELETE FROM paperboat.billing_products WHERE code=$4`,
			`DELETE FROM paperboat.users WHERE id=$5`,
		} {
			_, _ = store.SQL().ExecContext(context.Background(), q, workspace, x, team, product, owner)
		}
	})
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
	// The product's own allowance_bytes (1) is not what a team gets: the per-seat config is.
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref) VALUES($1,$1,'stripe',$1,$1,'personal_subscription','{"allowance_bytes":1}')`, product)
	at := time.Now().UTC().Truncate(time.Second)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end,bandwidth_anchor) VALUES($1,$2,'active',$3,4,$4,$5,$4)`, workspace, team, product, at.Add(-time.Hour), at.AddDate(0, 3, 0))

	s := billing.NewWorkspaceService(store, nil, "")
	fair := config.Default().Billing.FairUse
	s.SetThrottleRates(fair)
	overview := func() billing.Workspace {
		t.Helper()
		w, err := s.Overview(ctx, owner, team)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	const perSeat = 100_000_000_000
	w := overview()
	if w.AllowanceBytes == nil || *w.AllowanceBytes != 4*perSeat || w.AllowanceSeats != 4 || w.AllowancePerSeatBytes != perSeat || w.RelayIncludedBytes != 4*fair.RelayIncludedBytes {
		t.Fatalf("four seats: allowance %v, seats %d, per seat %d, relay %d; want %d, 4, %d, %d", w.AllowanceBytes, w.AllowanceSeats, w.AllowancePerSeatBytes, w.RelayIncludedBytes, 4*perSeat, perSeat, 4*fair.RelayIncludedBytes)
	}
	// A seat change mid-period applies at once, to the overview and to the period row every other reader uses.
	exec(`UPDATE paperboat.billing_workspaces SET quantity=6 WHERE id=$1`, workspace)
	if w = overview(); *w.AllowanceBytes != 6*perSeat || w.AllowanceSeats != 6 {
		t.Fatalf("six seats: allowance %d seats %d", *w.AllowanceBytes, w.AllowanceSeats)
	}
	var row int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT included_bytes FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=now() AND period_end>now()`, workspace).Scan(&row); err != nil || row != 6*perSeat {
		t.Fatalf("period row allowance = %d (%v), want %d", row, err, 6*perSeat)
	}
	// An operator's seats override counts like bought seats.
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := planlimits.GrantTx(ctx, tx, "operator_x", workspace, planlimits.Seats, 2, nil, "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w = overview(); *w.AllowanceBytes != 2*perSeat || w.AllowanceSeats != 2 {
		t.Fatalf("a seats override of 2: allowance %d seats %d", *w.AllowanceBytes, w.AllowanceSeats)
	}
	// The pool is one bucket: the team throttles only when all of it is used.
	debit := func(n int64) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, _, err := s.DebitWorkspaceBandwidthTx(ctx, tx, workspace, n, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	debit(perSeat + perSeat/2) // 150 of 200 GB, however many members or routes carried it
	if w = overview(); w.Throttled {
		t.Fatal("a team with 50 GB of its 200 GB pool left is throttled")
	}
	debit(perSeat)
	if w = overview(); !w.Throttled {
		t.Fatal("a team past its 200 GB pool is not throttled")
	}
}
