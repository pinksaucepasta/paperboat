package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// Bandwidth an operator granted is part of the workspace's allowance everywhere the allowance is read: what the customer's
// billing answer says, whether the debit admits traffic, and the exhaustion gate the hot paths use. It ends with the grant
// (revoked or past its end), and a workspace with an unlimited allowance stays unlimited.
func TestGrantedBandwidthIsPartOfTheAllowanceEverywherePostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, operator := "usr_gb_"+x, "operator_gb_"+x
	workspace := "personal:" + user
	insertUser(t, store, user, user+"@example.test")
	// A small personal allowance for this test; the policy row is shared, so it is put back.
	var before *int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT personal_allowance_bytes FROM paperboat.billing_operator_policy WHERE singleton`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=1000 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=$1 WHERE singleton`, before)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id=$1`, workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,user_id) VALUES($1,$2)`, workspace, user); err != nil {
		t.Fatal(err)
	}
	service := billing.NewWorkspaceService(store, nil, "")
	gate := bandwidthgate.New(store)
	debit := func(bytes int64) (granted int64, exhausted bool) {
		t.Helper()
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			granted, exhausted, err = service.DebitWorkspaceBandwidthTx(ctx, tx, workspace, bytes, time.Now().UTC())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	denied := func() bool {
		t.Helper()
		d, err := gate.Lookup(ctx, workspace, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	overview := func() billing.Workspace {
		t.Helper()
		w, err := service.Overview(ctx, user, "")
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	grant := func(amount int64) planlimits.Grant {
		t.Helper()
		var g planlimits.Grant
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			g, err = planlimits.GrantTx(ctx, tx, operator, workspace, planlimits.Bandwidth, amount, nil, "case:test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return g
	}

	if got, exhausted := debit(1000); got != 1000 || !exhausted {
		t.Fatalf("the plan's allowance: granted=%d exhausted=%t", got, exhausted)
	}
	if !denied() {
		t.Fatal("the gate does not deny an exhausted workspace")
	}
	if w := overview(); w.AllowanceBytes == nil || *w.AllowanceBytes != 1000 || w.AllowanceGrantedBytes != 0 || w.UsageBytes != 1000 {
		t.Fatalf("overview before a grant: allowance=%v granted=%d usage=%d", w.AllowanceBytes, w.AllowanceGrantedBytes, w.UsageBytes)
	}
	if got, _ := debit(10); got != 0 {
		t.Fatalf("traffic admitted past the allowance: %d", got)
	}

	// 500 bytes granted: admitted again, the gate lets it through, the customer sees allowance 1500 of which 500 granted.
	g := grant(500)
	if denied() {
		t.Fatal("the gate still denies after a grant")
	}
	if w := overview(); w.AllowanceBytes == nil || *w.AllowanceBytes != 1500 || w.AllowanceGrantedBytes != 500 {
		t.Fatalf("overview with a grant: allowance=%v granted=%d", w.AllowanceBytes, w.AllowanceGrantedBytes)
	}
	if got, exhausted := debit(300); got != 300 || exhausted {
		t.Fatalf("the granted bytes: granted=%d exhausted=%t", got, exhausted)
	}
	if got, exhausted := debit(400); got != 200 || !exhausted {
		t.Fatalf("the grant's last bytes: granted=%d exhausted=%t", got, exhausted)
	}
	if !denied() {
		t.Fatal("the gate lets an exhausted workspace through")
	}
	// A second grant adds; a revoke or an end takes bytes away again.
	grant(100)
	if denied() {
		t.Fatal("a second grant did not add")
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := planlimits.RevokeTx(ctx, tx, operator, workspace, g.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !denied() {
		t.Fatal("a revoked grant still counts at the gate")
	}
	if w := overview(); w.AllowanceBytes == nil || *w.AllowanceBytes != 1100 || w.AllowanceGrantedBytes != 100 {
		t.Fatalf("overview after a revoke: allowance=%v granted=%d", w.AllowanceBytes, w.AllowanceGrantedBytes)
	}
	store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_grants SET ends_at=now()-interval '1 second' WHERE workspace_id=$1 AND revoked_at IS NULL`, workspace)
	if w := overview(); w.AllowanceBytes == nil || *w.AllowanceBytes != 1000 || w.AllowanceGrantedBytes != 0 {
		t.Fatalf("overview after the grant ended: allowance=%v granted=%d", w.AllowanceBytes, w.AllowanceGrantedBytes)
	}

	// An unlimited allowance stays unlimited whatever is granted.
	store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=NULL WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	grant(777)
	if w := overview(); w.AllowanceBytes != nil || w.AllowanceGrantedBytes != 0 {
		t.Fatalf("an unlimited allowance reads as limited: %v granted=%d", w.AllowanceBytes, w.AllowanceGrantedBytes)
	}
}

// An operator's seat override replaces what the team bought: a full team can invite once seats are granted, the override
// can also lower the capacity, and the customer's billing answer shows it as granted.
func TestSeatOverrideReplacesTheBoughtSeatsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, member, guest, guest2, operator := "usr_ss_"+x, "usr_sm_"+x, "usr_sg_"+x, "usr_sh_"+x, "operator_ss_"+x
	team := "seat-ovr-" + x
	for _, u := range []string{owner, member, guest, guest2} {
		insertUser(t, store, u, u+"@example.test")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member)
	code := "seat-ovr-plan-" + x
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,$2,'team_subscription','{}',true)`, code, "price_"+x)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,provider_subscription_id,provider_item_id,provider_customer_id,product_code,period_start,period_end) VALUES('team:'||$1,$1,'active',2,'sub_'||$1,'si_'||$1,'cus_'||$1,$2,now()-interval '1 hour',now()+interval '1 day')`, team, code)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id=$1`, "team:"+team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_invitations WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
	})
	ts := teams.NewService(store)
	invite := func(op, target string) error {
		t.Helper()
		state, err := ts.Get(ctx, owner, team)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ts.Invite(ctx, owner, team, teams.InviteRequest{OperationID: op + x, ExpectedGeneration: state.Generation, AccountID: target})
		return err
	}
	grant := func(seats int64) planlimits.Grant {
		t.Helper()
		var g planlimits.Grant
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			g, err = planlimits.GrantTx(ctx, tx, operator, "team:"+team, planlimits.Seats, seats, nil, "case:test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	service := billing.NewWorkspaceService(store, nil, "")
	if err := invite("full", guest); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("2 seats, 2 members: %v", err)
	}
	g := grant(3)
	if err := invite("granted", guest); err != nil {
		t.Fatalf("an invitation within the granted seats: %v", err)
	}
	if err := invite("granted-full", guest2); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("beyond the granted seats: %v", err)
	}
	if w, err := service.Overview(ctx, owner, team); err != nil || w.SeatsGranted == nil || *w.SeatsGranted != 3 || w.Quantity != 2 {
		t.Fatalf("the customer's answer: granted=%v bought=%d %v", w.SeatsGranted, w.Quantity, err)
	}
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := planlimits.RevokeTx(ctx, tx, operator, "team:"+team, g.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := invite("revoked", guest2); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("after the revoke the bought seats bind again: %v", err)
	}
	if w, err := service.Overview(ctx, owner, team); err != nil || w.SeatsGranted != nil {
		t.Fatalf("a revoked seat grant still shows: %v %v", w.SeatsGranted, err)
	}
	// A grant below what the team uses deletes nobody: the members stay, only new invitations are refused.
	grant(1)
	if err := invite("lowered", guest2); !errors.Is(err, teams.ErrSeatRequired) {
		t.Fatalf("a lowered seat grant admitted an invitation: %v", err)
	}
	var members int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_members WHERE team_id=$1 AND active`, team).Scan(&members); err != nil || members != 2 {
		t.Fatalf("members after the lowered grant: %d %v", members, err)
	}
	if w, err := service.Overview(ctx, owner, team); err != nil || w.SeatsGranted == nil || *w.SeatsGranted != 1 {
		t.Fatalf("the lowered grant reads: %v %v", w.SeatsGranted, err)
	}
}
