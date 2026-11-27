package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

// L37: a team removes a seat (3 to 2 from renewal), cancels, then keeps the plan. Stripe released the
// reduction's schedule on cancel; keeping the plan must schedule the reduction again so it renews at 2.
func TestSeatReductionSurvivesCancelAndResume(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, team := "usr_resume_"+x, "seat-resume-"+x
	insertUser(t, store, owner, owner+"@example.test")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	code, price := "resume-plan-"+x, "price_resume_"+x
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,$2,'team_subscription','{}',true)`, code, price)
	start, end := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(24*time.Hour).UTC().Truncate(time.Second)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,provider_subscription_id,provider_item_id,provider_customer_id,product_code,period_start,period_end) VALUES('team:'||$1,$1,'active',3,'sub_'||$1,'si_'||$1,'cus_'||$1,$2,$3,$4)`, team, code, start, end)
	t.Cleanup(func() {
		c := context.Background()
		store.SQL().ExecContext(c, `DELETE FROM paperboat.stripe_events WHERE subscription_id=$1`, "sub_"+team)
		store.SQL().ExecContext(c, `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, "team:"+team)
		store.SQL().ExecContext(c, `DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, team)
		store.SQL().ExecContext(c, `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
	})
	sub := billing.StripeSubscription{ID: "sub_" + team, CustomerID: "cus_" + team, ItemID: "si_" + team, PriceID: price, Status: "active", Quantity: 3, PeriodStart: start, PeriodEnd: end}
	fake := &membershipStripeFake{subs: map[string]billing.StripeSubscription{sub.ID: sub}, fail: map[string]bool{}, updates: map[string]int{}}
	service := billing.NewWorkspaceService(store, fake, "seat-resume-test-key")
	if _, err := service.ChangeSeats(ctx, owner, team, 2, "reduce"+x); err != nil {
		t.Fatal(err)
	}
	n := 0
	webhook := func() {
		t.Helper()
		n++
		var event billing.StripeWebhook
		event.ID = fmt.Sprintf("evt_resume_%d_%s", n, x)
		event.Type = "customer.subscription.updated"
		event.Data.Object.ID = sub.ID
		event.Data.Object.Object = "subscription"
		if _, err := service.HandleWebhook(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	scheduled := func(want int, when string) {
		t.Helper()
		w, err := service.Overview(ctx, owner, team)
		if err != nil || w.Quantity != 3 || w.ScheduledQuantity == nil || *w.ScheduledQuantity != want {
			t.Fatalf("%s: %+v err=%v", when, w, err)
		}
	}
	scheduled(2, "after the reduction")
	// The customer cancels in the portal; Stripe releases the schedule.
	cancelled := fake.subs[sub.ID]
	cancelled.CancelAtPeriodEnd, cancelled.ScheduleID = true, ""
	fake.subs[sub.ID] = cancelled
	webhook()
	scheduled(2, "while cancelling")
	before := len(fake.reductions)
	// The customer keeps the plan.
	resumed := fake.subs[sub.ID]
	resumed.CancelAtPeriodEnd = false
	fake.subs[sub.ID] = resumed
	webhook()
	if len(fake.reductions) != before+1 {
		t.Fatalf("keeping the plan did not schedule the reduction again: %+v", fake.reductions)
	}
	if r := fake.reductions[before]; r.Quantity != 2 || r.PreviousQuantity != 3 || !r.PeriodEnd.Equal(end) || r.SubscriptionID != sub.ID {
		t.Fatalf("reapplied reduction=%+v", r)
	}
	if fake.subs[sub.ID].ScheduleID == "" {
		t.Fatal("no schedule on the subscription after resume")
	}
	scheduled(2, "after keeping the plan")
	// A repeat event changes nothing more.
	webhook()
	if len(fake.reductions) != before+1 {
		t.Fatalf("repeat event scheduled again: %+v", fake.reductions)
	}
	scheduled(2, "after a repeat event")
}
