package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// teamGrantFixture is a team (an owner and one more member) whose workspace holds a support plan grant of seats seats.
type teamGrantFixture struct {
	t                                          *testing.T
	ctx                                        context.Context
	store                                      *db.DB
	x, owner, member, team, workspace          string
	planCode, planPrice, topupCode, topupPrice string
	exec                                       func(string, ...any)
}

func newTeamGrantFixture(t *testing.T, seats int) *teamGrantFixture {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	f := &teamGrantFixture{t: t, ctx: ctx, store: store, x: x, owner: "usr_tg_" + x, member: "usr_tgm_" + x, team: "tg-" + x, planCode: "team-plan-" + x, planPrice: "price_tp_" + x, topupCode: "topup_" + x, topupPrice: "price_tu_" + x}
	f.workspace = "team:" + f.team
	insertUser(t, store, f.owner, f.owner+"@example.test")
	insertUser(t, store, f.member, f.member+"@example.test")
	f.exec = func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team, f.owner)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, f.team, f.owner, f.member)
	f.exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,$2,'stripe',$3,$4,'team_subscription','{"display_name":"Team"}',true,1500,'usd','month')`, "bp_tp_"+x, f.planCode, "prod_tp_"+x, f.planPrice)
	f.exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,$2,'stripe',$3,$4,'top_up','{"display_name":"100 GB top-up","bytes_amount":100000000000}',true,500,'usd','one_time')`, "bp_tu_"+x, f.topupCode, "prod_tu_"+x, f.topupPrice)
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`,
			`DELETE FROM paperboat.billing_workspaces WHERE id=$1`,
		} {
			store.SQL().ExecContext(bg, q, f.workspace)
		}
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, f.team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, f.team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_products WHERE code IN ($1,$2)`, f.planCode, f.topupCode)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, f.owner, f.member)
	})
	f.grant(seats)
	return f
}

// grant leaves the team workspace as the operator action does for a team that never paid.
func (f *teamGrantFixture) grant(seats int) {
	f.t.Helper()
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,product_code,state,quantity,period_start,period_end,cancel_at_period_end,grant_until) VALUES($1,$2,$3,'active',$4,now(),now()+interval '10 days',true,now()+interval '10 days')
 ON CONFLICT (id) DO UPDATE SET product_code=excluded.product_code,state='active',quantity=excluded.quantity,period_start=excluded.period_start,period_end=excluded.period_end,cancel_at_period_end=true,grant_until=excluded.grant_until,provider_subscription_id=NULL,provider_item_id=NULL`, f.workspace, f.team, f.planCode, seats)
}

// A team's own billing answer says its grant, until when, and the seats it grants; checkout over the grant sizes the
// subscription by the team's members, not by the grant's seats.
func TestTeamGrantOverviewAndCheckoutQuantity(t *testing.T) {
	f := newTeamGrantFixture(t, 5)
	fake := &workspaceStripeFake{}
	service := billing.NewWorkspaceService(f.store, fake, "workspace-test-key")
	w, err := service.Overview(f.ctx, f.owner, f.team)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Granted || w.GrantedUntil == nil || time.Until(*w.GrantedUntil) < 9*24*time.Hour || w.ProductCode != f.planCode || w.Quantity != 5 || w.Members != 2 || !w.CanManage {
		t.Fatalf("team grant overview: granted=%t until=%v product=%q seats=%d members=%d manage=%t", w.Granted, w.GrantedUntil, w.ProductCode, w.Quantity, w.Members, w.CanManage)
	}
	// A member who may not manage billing sees the same grant.
	if other, err := service.Overview(f.ctx, f.member, f.team); err != nil || !other.Granted || other.CanManage {
		t.Fatalf("member overview: granted=%t manage=%t err=%v", other.Granted, other.CanManage, err)
	}
	if _, err = service.Checkout(f.ctx, f.owner, f.owner+"@example.test", f.team, f.planCode, "checkout-"+f.x, "https://example.test/s", "https://example.test/c", 1); err != nil {
		t.Fatalf("checkout over a team grant: %v", err)
	}
	if len(fake.checkoutInputs) != 1 || fake.checkoutInputs[0].Quantity != 2 {
		t.Fatalf("a team checkout is sized by its members (2), got %+v", fake.checkoutInputs)
	}
	// The grant outlives the checkout until the subscription is paid.
	if w, err = service.Overview(f.ctx, f.owner, f.team); err != nil || !w.Granted {
		t.Fatalf("a started checkout ended the grant: granted=%t err=%v", w.Granted, err)
	}
}

// A running grant has no subscription to change seats on and no payer to buy top-ups with: both are refused, as they are
// for a plan whose subscription is gone, and a paid plan of the same team is not.
func TestSeatsAndTopupsAreRefusedWhileGranted(t *testing.T) {
	f := newTeamGrantFixture(t, 5)
	seatsFake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{}, fail: map[string]bool{}, updates: map[string]int{}}
	seats := billing.NewWorkspaceService(f.store, seatsFake, "seats-test-key")
	if _, err := seats.PreviewSeats(f.ctx, f.owner, f.team, 6); !errors.Is(err, billing.ErrSeatSubscription) {
		t.Fatalf("seat preview on a grant: %v", err)
	}
	if _, err := seats.ChangeSeats(f.ctx, f.owner, f.team, 6, "seats-"+f.x); !errors.Is(err, billing.ErrSeatSubscription) {
		t.Fatalf("seat change on a grant: %v", err)
	}
	topupFake := &topupStripeFake{states: map[string]billing.StripeTopupState{}}
	topups := billing.NewWorkspaceService(f.store, topupFake, "topup-test-key")
	topup := func(key string) error {
		_, err := topups.Topup(f.ctx, f.owner, f.team, f.topupCode, key, "https://example.test/s", "https://example.test/c", 1, 1)
		return err
	}
	if err := topup("granted-" + f.x); !errors.Is(err, billing.ErrTopupRequiresPaid) {
		t.Fatalf("a top-up on a grant: %v", err)
	}
	// A grant given over a lapsed Stripe customer is refused the same way, however paid-shaped its row is.
	f.exec(`UPDATE paperboat.billing_workspaces SET provider_customer_id='cus_'||$2 WHERE id=$1`, f.workspace, f.x)
	if err := topup("granted-customer-" + f.x); !errors.Is(err, billing.ErrTopupRequiresPaid) {
		t.Fatalf("a top-up on a grant over a lapsed customer: %v", err)
	}
	if topupFake.calls != 0 {
		t.Fatalf("Stripe was called for a refused top-up: %d", topupFake.calls)
	}

	// The same team once it really pays: a subscription, the grant gone. Top-ups and seat changes work.
	start, end := time.Now().Add(-time.Hour).UTC(), time.Now().Add(30*24*time.Hour).UTC()
	f.exec(`UPDATE paperboat.billing_workspaces SET grant_until=NULL,cancel_at_period_end=false,provider_subscription_id=$2,provider_item_id=$3,period_start=$4,period_end=$5 WHERE id=$1`, f.workspace, "sub_"+f.x, "si_"+f.x, start, end)
	sub := billing.StripeSubscription{ID: "sub_" + f.x, CustomerID: "cus_" + f.x, ItemID: "si_" + f.x, PriceID: f.planPrice, Status: "active", Quantity: 5, PeriodStart: start, PeriodEnd: end}
	seatsFake.subs[sub.ID] = sub
	if _, err := seats.PreviewSeats(f.ctx, f.owner, f.team, 6); err != nil {
		t.Fatalf("a paid team's seat preview: %v", err)
	}
	if err := topup("paid-" + f.x); err != nil {
		t.Fatalf("a paid team's top-up: %v", err)
	}
	if topupFake.calls != 1 {
		t.Fatalf("one Stripe top-up checkout expected: %d", topupFake.calls)
	}
}

// Each reason a team cannot change seats refuses on its own: no subscription, no item, a cancelling plan. A grant has all
// of them, so a future grant shape that kept one would still be refused by the rest; this pins each guard separately.
func TestEachSeatChangeGuardRefusesOnItsOwn(t *testing.T) {
	f := newTeamGrantFixture(t, 5)
	start, end := time.Now().Add(-time.Hour).UTC(), time.Now().Add(30*24*time.Hour).UTC()
	sub := billing.StripeSubscription{ID: "sub_" + f.x, CustomerID: "cus_" + f.x, ItemID: "si_" + f.x, PriceID: f.planPrice, Status: "active", Quantity: 5, PeriodStart: start, PeriodEnd: end}
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{sub.ID: sub}, fail: map[string]bool{}, updates: map[string]int{}}
	service := billing.NewWorkspaceService(f.store, fake, "seats-test-key")
	// A healthy paid plan, then one fault at a time.
	reset := func(sql string) {
		f.exec(`UPDATE paperboat.billing_workspaces SET grant_until=NULL,state='active',cancel_at_period_end=false,provider_customer_id='cus_'||$2,provider_subscription_id='sub_'||$2,provider_item_id='si_'||$2,period_start=$3,period_end=$4 WHERE id=$1`, f.workspace, f.x, start, end)
		if sql != "" {
			f.exec(sql, f.workspace)
		}
	}
	reset("")
	if _, err := service.PreviewSeats(f.ctx, f.owner, f.team, 6); err != nil {
		t.Fatalf("a healthy paid plan: %v", err)
	}
	for name, fault := range map[string]string{
		"no subscription": `UPDATE paperboat.billing_workspaces SET provider_subscription_id=NULL WHERE id=$1`,
		"no item":         `UPDATE paperboat.billing_workspaces SET provider_item_id=NULL WHERE id=$1`,
		"cancelling plan": `UPDATE paperboat.billing_workspaces SET cancel_at_period_end=true WHERE id=$1`,
		"cancelled plan":  `UPDATE paperboat.billing_workspaces SET state='canceled' WHERE id=$1`,
		"ended period":    `UPDATE paperboat.billing_workspaces SET period_start=now()-interval '2 hours',period_end=now()-interval '1 hour' WHERE id=$1`,
	} {
		reset(fault)
		if _, err := service.PreviewSeats(f.ctx, f.owner, f.team, 6); !errors.Is(err, billing.ErrSeatSubscription) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A team that pays while it holds a grant: the paid subscription (the team catalog's product, sized by the team) is
// applied and ends the grant, and an old subscription's cancelled replay does not touch the grant, for a team as for a person.
func TestTeamGrantEndsWhenTheTeamPaysAndSurvivesAnOldReplay(t *testing.T) {
	f := newTeamGrantFixture(t, 5)
	f.exec(`UPDATE paperboat.billing_workspaces SET provider_customer_id='cus_'||$2 WHERE id=$1`, f.workspace, f.x)
	op := func(key string) {
		request, _ := json.Marshal(map[string]any{"CustomerID": "cus_" + f.x, "PriceID": f.planPrice, "PredecessorSubscriptionID": "", "Quantity": 2, "SuccessURL": "https://example.test/s", "CancelURL": "https://example.test/c"})
		cipher, err := secrets.Encrypt("workspace-test-key", string(request))
		if err != nil {
			t.Fatal(err)
		}
		f.exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'checkout','h','succeeded',$4)`, key, f.workspace, f.owner, cipher)
	}
	op("checkout-old-" + f.x)
	op("checkout-new-" + f.x)
	fake := &workspaceStripeFake{}
	service := billing.NewWorkspaceService(f.store, fake, "workspace-test-key")
	deliver := func(sub billing.StripeSubscription, n int) {
		t.Helper()
		fake.sub = sub
		event, err := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":"evt_tg_%s_%d","type":"customer.subscription.updated","data":{"object":{"id":%q,"object":"subscription"}}}`, f.x, n, sub.ID)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id=$1`, fmt.Sprintf("evt_tg_%s_%d", f.x, n))
		})
		if processed, err := service.HandleWebhook(f.ctx, event); err != nil || !processed {
			t.Fatalf("processed=%t err=%v", processed, err)
		}
	}
	overview := func() billing.Workspace {
		t.Helper()
		w, err := service.Overview(f.ctx, f.owner, f.team)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	deliver(billing.StripeSubscription{ID: "sub_old_" + f.x, CustomerID: "cus_" + f.x, PriceID: f.planPrice, ItemID: "si_old", Status: "canceled", CheckoutOperation: "checkout-old-" + f.x, Quantity: 2, PeriodStart: time.Now().Add(-90 * 24 * time.Hour), PeriodEnd: time.Now().Add(-60 * 24 * time.Hour)}, 1)
	if w := overview(); !w.Granted || w.Quantity != 5 {
		t.Fatalf("an old subscription's replay touched the team grant: granted=%t seats=%d", w.Granted, w.Quantity)
	}
	deliver(billing.StripeSubscription{ID: "sub_new_" + f.x, CustomerID: "cus_" + f.x, PriceID: f.planPrice, ItemID: "si_new", Status: "active", CheckoutOperation: "checkout-new-" + f.x, Quantity: 2, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now().Add(30 * 24 * time.Hour)}, 2)
	if w := overview(); w.Granted || w.Quantity != 2 || w.State != "active" {
		t.Fatalf("a paying team still reads as granted: granted=%t seats=%d state=%q", w.Granted, w.Quantity, w.State)
	}
}

// A workspace reads as granted only while its grant is the plan: a running grant_until on a paid, cancelled or ended row is not one.
func TestGrantedMeansARunningGrantAndNothingElse(t *testing.T) {
	f := newTeamGrantFixture(t, 5)
	service := billing.NewWorkspaceService(f.store, &workspaceStripeFake{}, "workspace-test-key")
	granted := func() bool {
		t.Helper()
		w, err := service.Overview(f.ctx, f.owner, f.team)
		if err != nil {
			t.Fatal(err)
		}
		return w.Granted
	}
	if !granted() {
		t.Fatal("a running grant is not granted")
	}
	for name, change := range map[string]string{
		"has a Stripe subscription": `UPDATE paperboat.billing_workspaces SET provider_subscription_id='sub_x' WHERE id=$1`,
		"is cancelled":              `UPDATE paperboat.billing_workspaces SET state='canceled' WHERE id=$1`,
		"has ended":                 `UPDATE paperboat.billing_workspaces SET grant_until=now()-interval '1 second' WHERE id=$1`,
		"was never marked":          `UPDATE paperboat.billing_workspaces SET grant_until=NULL WHERE id=$1`,
	} {
		f.grant(5)
		f.exec(change, f.workspace)
		if granted() {
			t.Fatalf("a workspace that %s reads as granted", name)
		}
	}
}
