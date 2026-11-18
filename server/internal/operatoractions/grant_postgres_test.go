package operatoractions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/operator"
)

// A plan grant makes the workspace's own subscription row an active plan with no Stripe subscription: the same
// readers see it as a paid plan, it ends by itself, a real subscription or an in-flight checkout blocks it, and a
// revoke ends only a grant.
func TestPlanGrantAndRevokeThroughTheWorkspaceRow(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "operator_" + uuid.NewString()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, admin)
	user, team := "grant_user_"+x, "grant_team_"+x
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role) VALUES($1,$1,$1||'@test.invalid','active','user')`, user)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	product := func(code, catalog string, active bool) {
		exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe','prod_'||$1,'price_'||$1,$3,'{}',$4)`, "bp_"+code, code, catalog, active)
	}
	pro, teamPlan, off := "pro-"+x, "team-"+x, "pro-off-"+x
	product(pro, "personal_subscription", true)
	product(teamPlan, "team_subscription", true)
	product(off, "personal_subscription", false)

	personal, teamWorkspace := "personal:"+user, "team:"+team
	run := func(actor, key, kind, target string, in Input) (Result, error) {
		return s.Execute(ctx, actor, key, kind, target, "case:test", in)
	}
	paid := func(workspace string) bool {
		var ok bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(billing_workspace_paid_until(w)>now(),false) FROM paperboat.billing_workspaces w WHERE id=$1`, workspace).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	row := func(workspace string) (product string, state string, subscription bool, revision int64) {
		var p, sub *string
		if err := store.SQL().QueryRowContext(ctx, `SELECT product_code,state,provider_subscription_id,revision FROM paperboat.billing_workspaces WHERE id=$1`, workspace).Scan(&p, &state, &sub, &revision); err != nil {
			t.Fatal(err)
		}
		if p != nil {
			product = *p
		}
		return product, state, sub != nil, revision
	}

	// Refusals: not an admin, bad shapes, wrong plan for the scope, unknown or inactive product.
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','billing')`, "operator_billing_"+x)
	if _, err := run("operator_billing_"+x, "g-role-"+x, "plan_grant", personal, Input{Revision: 1, ProductCode: pro, Days: 30}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-admin granted a plan: %v", err)
	}
	for name, in := range map[string]Input{
		"no days":               {Revision: 1, ProductCode: pro},
		"too long":              {Revision: 1, ProductCode: pro, Days: 400},
		"no product":            {Revision: 1, Days: 30},
		"seats on personal":     {Revision: 1, ProductCode: pro, Days: 30, Quantity: 3},
		"team plan on personal": {Revision: 1, ProductCode: teamPlan, Days: 30},
	} {
		if _, err := run(admin, "g-bad-"+name+x, "plan_grant", personal, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := run(admin, "g-nocode-"+x, "plan_grant", personal, Input{Revision: 1, ProductCode: "pro-missing-" + x, Days: 30}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown product: %v", err)
	}
	if _, err := run(admin, "g-off-"+x, "plan_grant", personal, Input{Revision: 1, ProductCode: off, Days: 30}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive product: %v", err)
	}
	if _, err := run(admin, "g-many-"+x, "plan_grant", teamWorkspace, Input{Revision: 1, ProductCode: teamPlan, Days: 30, Quantity: 129}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("more seats than a team can have: %v", err)
	}
	if _, err := run(admin, "g-seats-"+x, "plan_grant", teamWorkspace, Input{Revision: 1, ProductCode: teamPlan, Days: 30}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a team plan without seats: %v", err)
	}
	if _, err := run(admin, "g-who-"+x, "plan_grant", "personal:nobody_"+x, Input{Revision: 1, ProductCode: pro, Days: 30}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := run(admin, "g-target-"+x, "plan_grant", "workspace_"+x, Input{Revision: 1, ProductCode: pro, Days: 30}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a target that is not a workspace id: %v", err)
	}

	// A personal grant: the workspace row is created, active, no Stripe subscription, paid for 30 days.
	granted, err := run(admin, "g-1-"+x, "plan_grant", personal, Input{Revision: 1, ProductCode: pro, Days: 30})
	if err != nil || granted.State != "granted" || granted.Version != 2 || granted.ExpiresAt == nil || time.Until(*granted.ExpiresAt) < 29*24*time.Hour {
		t.Fatalf("grant: %+v %v", granted, err)
	}
	if p, state, sub, rev := row(personal); p != pro || state != "active" || sub || rev != 2 || !paid(personal) {
		t.Fatalf("row: %s %s sub=%t rev=%d paid=%t", p, state, sub, rev, paid(personal))
	}
	if replay, err := run(admin, "g-1-"+x, "plan_grant", personal, Input{Revision: 1, ProductCode: pro, Days: 30}); err != nil || replay.ExpiresAt == nil || !replay.ExpiresAt.Equal(*granted.ExpiresAt) || replay.Version != granted.Version {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = run(admin, "g-stale-"+x, "plan_grant", personal, Input{Revision: 1, ProductCode: pro, Days: 60}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale revision: %v", err)
	}
	var events int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='operator.plan_grant' AND resource_id=$1 AND metadata->>'product_code'=$2 AND resource_type='billing_workspace'`, personal, pro).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit events=%d %v", events, err)
	}
	// A new grant replaces the old one; revoke ends it; a second revoke finds no grant.
	if again, err := run(admin, "g-2-"+x, "plan_grant", personal, Input{Revision: 2, ProductCode: pro, Days: 90}); err != nil || again.Version != 3 {
		t.Fatalf("regrant: %+v %v", again, err)
	}
	exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'checkout','h','pending')`, "op_personal_"+x, personal, user)
	if _, err = run(admin, "r-pending-"+x, "plan_revoke", personal, Input{Revision: 3}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a revoke while a checkout is pending: %v", err)
	}
	exec(`UPDATE paperboat.billing_workspace_operations SET state='succeeded' WHERE idempotency_key=$1`, "op_personal_"+x)
	if revoked, err := run(admin, "r-1-"+x, "plan_revoke", personal, Input{Revision: 3}); err != nil || revoked.State != "revoked" || paid(personal) {
		t.Fatalf("revoke: %+v paid=%t %v", revoked, paid(personal), err)
	}
	// A workspace that never had a Stripe customer goes back to a plain free one.
	if p, state, _, _ := row(personal); p != "" || state != "free" {
		t.Fatalf("after revoking a pure grant: product=%q state=%s", p, state)
	}
	if _, err = run(admin, "r-2-"+x, "plan_revoke", personal, Input{Revision: 4}); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoking what is not a grant: %v", err)
	}
	if _, err = run(admin, "r-days-"+x, "plan_revoke", personal, Input{Revision: 4, Days: 3}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant fields on a revoke: %v", err)
	}

	// A grant ends by itself.
	exec(`UPDATE paperboat.billing_workspaces SET state='active',product_code=$2,period_start=now()-interval '2 seconds',period_end=now()-interval '1 second' WHERE id=$1`, personal, pro)
	if paid(personal) {
		t.Fatal("an expired grant still counts as paid")
	}

	// The operator read says whether the plan is a running grant.
	reader := operator.NewService(store)
	isGranted := func() bool {
		v, err := reader.Read(ctx, auth.User{ID: admin}, "billing", personal, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		return v.(operator.Billing).Granted
	}
	exec(`UPDATE paperboat.billing_workspaces SET state='active',product_code=$2,provider_subscription_id=NULL,period_start=now(),period_end=now()+interval '5 days',grant_until=now()+interval '5 days' WHERE id=$1`, personal, pro)
	if !isGranted() {
		t.Fatal("a running grant is not reported as one")
	}
	exec(`UPDATE paperboat.billing_workspaces SET period_start=now()-interval '2 seconds',period_end=now()-interval '1 second',grant_until=now()-interval '1 second' WHERE id=$1`, personal)
	if isGranted() {
		t.Fatal("an expired grant is still reported as one")
	}

	// A lapsed Stripe record can be comped, and the grant clears its subscription id; a live one cannot.
	exec(`UPDATE paperboat.billing_workspaces SET provider_customer_id='cus_'||$2,provider_subscription_id='sub_old_'||$2,provider_item_id='si_'||$2,state='canceled',scheduled_quantity=2,scheduled_quantity_at=now(),pending_seat_capacity=4,updated_at=now() WHERE id=$1`, personal, x)
	var rev int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT revision FROM paperboat.billing_workspaces WHERE id=$1`, personal).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	// A cancelled Stripe record is granted over at once (the running grant, not a wait, protects it from old events).
	if _, err = run(admin, "g-lapsed-"+x, "plan_grant", personal, Input{Revision: rev, ProductCode: pro, Days: 10}); err != nil {
		t.Fatalf("a grant over a cancelled Stripe record: %v", err)
	}
	var running bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT grant_until>now()+interval '9 days' FROM paperboat.billing_workspaces WHERE id=$1`, personal).Scan(&running); err != nil || !running {
		t.Fatalf("grant_until not set: %t %v", running, err)
	}
	var leftover int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.billing_workspaces WHERE id=$1 AND (scheduled_quantity IS NOT NULL OR scheduled_quantity_at IS NOT NULL OR pending_seat_capacity IS NOT NULL)`, personal).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatalf("seat scheduling left over after a grant: %d %v", leftover, err)
	}
	var customer, subscription, item *string
	if err = store.SQL().QueryRowContext(ctx, `SELECT provider_customer_id,provider_subscription_id,provider_item_id FROM paperboat.billing_workspaces WHERE id=$1`, personal).Scan(&customer, &subscription, &item); err != nil || customer == nil || subscription != nil || item != nil {
		t.Fatalf("customer=%v subscription=%v item=%v %v", customer, subscription, item, err)
	}
	// A Stripe subscription (or a checkout in flight) owns the workspace: no grant, and no revoke of it.
	exec(`UPDATE paperboat.billing_workspaces SET provider_customer_id='cus_'||$2,provider_subscription_id='sub_'||$2,state='active',period_start=now(),period_end=now()+interval '30 days' WHERE id=$1`, personal, x)
	if err = store.SQL().QueryRowContext(ctx, `SELECT revision FROM paperboat.billing_workspaces WHERE id=$1`, personal).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if _, err = run(admin, "g-sub-"+x, "plan_grant", personal, Input{Revision: rev, ProductCode: pro, Days: 30}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant over a Stripe subscription: %v", err)
	}
	if _, err = run(admin, "r-sub-"+x, "plan_revoke", personal, Input{Revision: rev}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a revoke of a Stripe subscription: %v", err)
	}
	if _, state, sub, _ := row(personal); state != "active" || !sub {
		t.Fatalf("the subscription was touched: %s %t", state, sub)
	}

	// A team grant gives the team its paid entitlement with the seats; revoke takes it away; a checkout in flight blocks.
	teamPaid := func() bool {
		var ok bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.team_paid_entitled($1)`, team).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if teamPaid() {
		t.Fatal("a team with no plan is entitled")
	}
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, teamWorkspace, team)
	exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'checkout','h','pending')`, "op_"+x, teamWorkspace, user)
	if _, err = run(admin, "t-pending-"+x, "plan_grant", teamWorkspace, Input{Revision: 1, ProductCode: teamPlan, Days: 30, Quantity: 5}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a grant while a checkout is pending: %v", err)
	}
	exec(`UPDATE paperboat.billing_workspace_operations SET state='succeeded' WHERE idempotency_key=$1`, "op_"+x)
	if _, err = run(admin, "t-1-"+x, "plan_grant", teamWorkspace, Input{Revision: 1, ProductCode: teamPlan, Days: 30, Quantity: 5}); err != nil || !teamPaid() {
		t.Fatalf("team grant: paid=%t %v", teamPaid(), err)
	}
	var seats int
	if err = store.SQL().QueryRowContext(ctx, `SELECT quantity FROM paperboat.billing_workspaces WHERE id=$1`, teamWorkspace).Scan(&seats); err != nil || seats != 5 {
		t.Fatalf("seats=%d %v", seats, err)
	}
	if _, err = run(admin, "t-r-"+x, "plan_revoke", teamWorkspace, Input{Revision: 2}); err != nil || teamPaid() {
		t.Fatalf("team revoke: paid=%t %v", teamPaid(), err)
	}
	// An incomplete_expired Stripe record can be granted over too, and a revoke ends the running grant.
	exec(`UPDATE paperboat.billing_workspaces SET provider_customer_id='cus_t_'||$2,provider_subscription_id='sub_t_'||$2,state='incomplete_expired',updated_at=now() WHERE id=$1`, teamWorkspace, x)
	var teamRev int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT revision FROM paperboat.billing_workspaces WHERE id=$1`, teamWorkspace).Scan(&teamRev); err != nil {
		t.Fatal(err)
	}
	if _, err = run(admin, "t-2-"+x, "plan_grant", teamWorkspace, Input{Revision: teamRev, ProductCode: teamPlan, Days: 2, Quantity: 2}); err != nil || !teamPaid() {
		t.Fatalf("a grant over an incomplete_expired record: paid=%t %v", teamPaid(), err)
	}
	if _, err = run(admin, "t-r2-"+x, "plan_revoke", teamWorkspace, Input{Revision: teamRev + 1}); err != nil || teamPaid() {
		t.Fatalf("revoke: paid=%t %v", teamPaid(), err)
	}
	var cleared bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT grant_until IS NULL FROM paperboat.billing_workspaces WHERE id=$1`, teamWorkspace).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("a revoke left the grant running: %t %v", cleared, err)
	}
}
