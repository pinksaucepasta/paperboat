package billing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// A support plan grant owns the workspace's plan while it runs (grant_until): a Stripe snapshot that is not a live paid
// subscription is not applied, however old the event carrying it (an automatic retry or a manual replay of an old
// subscription's event), while a subscription the customer really pays for is applied and ends the grant.
func TestPlanGrantIsNotOverwrittenByAnOldSubscriptionsEvent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, code, price := "usr_grant_"+x, "grant-plan-"+x, "price_grant_"+x
	workspace, customer := "personal:"+user, "cus_"+x
	insertUser(t, store, user, user+"@example.test")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'personal_subscription','{}',true)`, "bp_grant_"+x, code, "prod_"+x, price)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.stripe_events WHERE id LIKE $1`, "evt_grant_"+x+"%")
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	checkoutOp := func(key, predecessor string) {
		t.Helper()
		request, _ := json.Marshal(map[string]any{"CustomerID": customer, "PriceID": price, "PredecessorSubscriptionID": predecessor, "Quantity": 1, "SuccessURL": "https://example.test/s", "CancelURL": "https://example.test/c"})
		cipher, err := secrets.Encrypt("workspace-test-key", string(request))
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'checkout','h','succeeded',$4)`, key, workspace, user, cipher)
	}
	fake := &workspaceStripeFake{}
	service := billing.NewWorkspaceService(store, fake, "workspace-test-key")
	n := 0
	replay := func(sub billing.StripeSubscription) bool {
		t.Helper()
		n++
		fake.sub = sub
		event, err := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":"customer.subscription.updated","data":{"object":{"id":%q,"object":"subscription"}}}`, fmt.Sprintf("evt_grant_%s_%d", x, n), sub.ID)))
		if err != nil {
			t.Fatal(err)
		}
		processed, err := service.HandleWebhook(ctx, event)
		if err != nil || !processed {
			t.Fatalf("processed=%t err=%v", processed, err)
		}
		return processed
	}
	row := func() (sub, product, state string, grantRunning bool) {
		t.Helper()
		var s, p *string
		if err := store.SQL().QueryRowContext(ctx, `SELECT provider_subscription_id,product_code,state,coalesce(grant_until>now(),false) FROM paperboat.billing_workspaces WHERE id=$1`, workspace).Scan(&s, &p, &state, &grantRunning); err != nil {
			t.Fatal(err)
		}
		if s != nil {
			sub = *s
		}
		if p != nil {
			product = *p
		}
		return
	}
	old := func(status string) billing.StripeSubscription {
		return billing.StripeSubscription{ID: "sub_old_" + x, CustomerID: customer, PriceID: price, ItemID: "si_old_" + x, Status: status, CheckoutOperation: "checkout-old-" + x, Quantity: 1, PeriodStart: time.Now().Add(-90 * 24 * time.Hour), PeriodEnd: time.Now().Add(-60 * 24 * time.Hour)}
	}
	// The workspace had a subscription bought by a checkout, which lapsed.
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'si_old','`+code+`','canceled',1,now()-interval '90 days',now()-interval '60 days')`, workspace, user, customer, "sub_old_"+x)
	checkoutOp("checkout-old-"+x, "")
	// Control: with no grant, the old subscription's snapshot is accepted by every checkout guard and rewrites the row.
	exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id=NULL,state='free' WHERE id=$1`, workspace)
	replay(old("canceled"))
	if sub, _, state, _ := row(); sub != "sub_old_"+x || state != "canceled" {
		t.Fatalf("control: the old subscription was not applied to a row with no grant: %s %s", sub, state)
	}

	// The grant, as the operator action leaves the row.
	grant := func(days int) {
		t.Helper()
		exec(`UPDATE paperboat.billing_workspaces SET product_code=$2,state='active',quantity=0,period_start=now(),period_end=now()+make_interval(days => $3),cancel_at_period_end=true,grant_until=now()+make_interval(days => $3),provider_subscription_id=NULL,provider_item_id=NULL WHERE id=$1`, workspace, code, days)
	}
	granted := func(label string) {
		t.Helper()
		if sub, product, state, running := row(); sub != "" || product != code || state != "active" || !running {
			t.Fatalf("%s: the grant was overwritten: sub=%q product=%q state=%q running=%t", label, sub, product, state, running)
		}
	}
	grant(10)
	// A webhook right after the grant, and replays of any age and in any non-live state, change nothing.
	for _, status := range []string{"canceled", "incomplete_expired", "incomplete", "unpaid", "past_due", "paused"} {
		replay(old(status))
		granted("replay of " + status)
	}
	exec(`UPDATE paperboat.billing_workspaces SET period_start=now()-interval '9 days',updated_at=now()-interval '9 days' WHERE id=$1`, workspace)
	replay(old("canceled"))
	granted("replay long after the grant")
	var recorded int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.stripe_events WHERE id LIKE $1`, "evt_grant_"+x+"%").Scan(&recorded); err != nil || recorded < 8 {
		t.Fatalf("ignored events must be recorded so Stripe stops retrying them: %d %v", recorded, err)
	}

	// The customer really subscribes (a newer checkout, no predecessor): the subscription is applied and the grant ends.
	checkoutOp("checkout-new-"+x, "")
	fresh := billing.StripeSubscription{ID: "sub_new_" + x, CustomerID: customer, PriceID: price, ItemID: "si_new_" + x, Status: "active", CheckoutOperation: "checkout-new-" + x, Quantity: 1, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now().Add(30 * 24 * time.Hour)}
	replay(fresh)
	if sub, _, state, running := row(); sub != "sub_new_"+x || state != "active" || running {
		t.Fatalf("a paid subscription must replace the grant: sub=%q state=%q grant running=%t", sub, state, running)
	}

	// A trial the customer starts ends a grant the same way.
	grant(10)
	checkoutOp("checkout-trial-"+x, "")
	trial := fresh
	trial.ID, trial.ItemID, trial.Status, trial.CheckoutOperation = "sub_trial_"+x, "si_trial_"+x, "trialing", "checkout-trial-"+x
	replay(trial)
	if sub, _, state, running := row(); sub != "sub_trial_"+x || state != "trialing" || running {
		t.Fatalf("a trial must replace the grant: sub=%q state=%q running=%t", sub, state, running)
	}

	// A checkout started before the grant (it names the lapsed subscription as predecessor, an id the grant cleared) still
	// applies when its subscription turns active: the customer paid, and a grant must not swallow that.
	exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id='sub_lapsed_'||$2,state='canceled' WHERE id=$1`, workspace, x)
	checkoutOp("checkout-before-"+x, "sub_lapsed_"+x)
	exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id=NULL,provider_item_id=NULL,state='active',grant_until=now()+interval '10 days',period_start=now(),period_end=now()+interval '10 days' WHERE id=$1`, workspace)
	before := fresh
	before.ID, before.ItemID, before.CheckoutOperation = "sub_before_"+x, "si_before_"+x, "checkout-before-"+x
	replay(before)
	if sub, _, state, running := row(); sub != "sub_before_"+x || state != "active" || running {
		t.Fatalf("a checkout started before the grant must still apply: sub=%q state=%q running=%t", sub, state, running)
	}

	// An expired grant (both its end and its period are past) protects nothing, and the lapsed snapshot is applied.
	exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id=NULL,state='active',period_end=now()-interval '1 second',grant_until=now()-interval '1 second' WHERE id=$1`, workspace)
	replay(old("canceled"))
	if sub, _, state, running := row(); sub != "sub_old_"+x || state != "canceled" || running {
		t.Fatalf("expired grant: sub=%q state=%q running=%t", sub, state, running)
	}
}
