package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// grantFixture is a personal workspace whose Stripe subscription lapsed, in the state a plan grant leaves it, with a
// service whose Stripe read returns whatever the test sets in fake.sub (the live subscription).
type grantFixture struct {
	t                                         *testing.T
	ctx                                       context.Context
	x, user, code, price, workspace, customer string
	fake                                      *workspaceStripeFake
	service                                   *billing.WorkspaceService
	exec                                      func(string, ...any)
	events                                    int
	fresh                                     billing.StripeSubscription
	row                                       func() (sub, product, state string, running bool)
	storeQueryCount                           func(query string, args ...any) int
}

func newGrantFixture(t *testing.T) *grantFixture {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	f := &grantFixture{t: t, ctx: ctx, x: x, user: "usr_grant_" + x, code: "grant-plan-" + x, price: "price_grant_" + x, customer: "cus_" + x}
	f.workspace = "personal:" + f.user
	insertUser(t, store, f.user, f.user+"@example.test")
	f.exec = func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'personal_subscription','{}',true)`, "bp_grant_"+x, f.code, "prod_"+x, f.price)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.stripe_events WHERE id LIKE $1`, "evt_grant_"+x+"%")
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, f.workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, f.workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, f.workspace)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_products WHERE code=$1`, f.code)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, f.user)
	})
	f.fake = &workspaceStripeFake{}
	f.service = billing.NewWorkspaceService(store, f.fake, "workspace-test-key")
	f.row = func() (sub, product, state string, running bool) {
		t.Helper()
		var s, p *string
		if err := store.SQL().QueryRowContext(ctx, `SELECT provider_subscription_id,product_code,state,coalesce(grant_until>now(),false) FROM paperboat.billing_workspaces WHERE id=$1`, f.workspace).Scan(&s, &p, &state, &running); err != nil {
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
	f.storeQueryCount = func(query string, args ...any) int {
		var n int
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The workspace had a subscription bought by a checkout; it lapsed.
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,provider_item_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'si_old','`+f.code+`','canceled',1,now()-interval '90 days',now()-interval '60 days')`, f.workspace, f.user, f.customer, "sub_old_"+x)
	f.checkoutOp("checkout-old-"+x, "")
	f.fresh = billing.StripeSubscription{ID: "sub_new_" + x, CustomerID: f.customer, PriceID: f.price, ItemID: "si_new_" + x, Status: "active", CheckoutOperation: "checkout-new-" + x, Quantity: 1, PeriodStart: time.Now().Add(-time.Hour), PeriodEnd: time.Now().Add(30 * 24 * time.Hour)}
	return f
}

// checkoutOp records a succeeded checkout of the workspace that names the predecessor subscription it replaced.
func (f *grantFixture) checkoutOp(key, predecessor string) {
	f.t.Helper()
	request, _ := json.Marshal(map[string]any{"CustomerID": f.customer, "PriceID": f.price, "PredecessorSubscriptionID": predecessor, "Quantity": 1, "SuccessURL": "https://example.test/s", "CancelURL": "https://example.test/c"})
	cipher, err := secrets.Encrypt("workspace-test-key", string(request))
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state,request_ciphertext) VALUES($1,$2,$3,'checkout','h','succeeded',$4)`, key, f.workspace, f.user, cipher)
}

func (f *grantFixture) old(status string) billing.StripeSubscription {
	return billing.StripeSubscription{ID: "sub_old_" + f.x, CustomerID: f.customer, PriceID: f.price, ItemID: "si_old_" + f.x, Status: status, CheckoutOperation: "checkout-old-" + f.x, Quantity: 1, PeriodStart: time.Now().Add(-90 * 24 * time.Hour), PeriodEnd: time.Now().Add(-60 * 24 * time.Hour)}
}

// grant leaves the row as the operator action does.
func (f *grantFixture) grant(days int) {
	f.t.Helper()
	f.exec(`UPDATE paperboat.billing_workspaces SET product_code=$2,state='active',quantity=0,period_start=now(),period_end=now()+make_interval(days => $3),cancel_at_period_end=true,grant_until=now()+make_interval(days => $3),provider_subscription_id=NULL,provider_item_id=NULL WHERE id=$1`, f.workspace, f.code, days)
}

func (f *grantFixture) granted(label string) {
	f.t.Helper()
	if sub, product, state, running := f.row(); sub != "" || product != f.code || state != "active" || !running {
		f.t.Fatalf("%s: the grant was overwritten: sub=%q product=%q state=%q running=%t", label, sub, product, state, running)
	}
}

// deliver sends a webhook for sub whose payload is body (a JSON object body fragment; "" for the minimal one) while
// Stripe's live read answers live. It returns what HandleWebhook returned.
func (f *grantFixture) deliver(live billing.StripeSubscription, eventType, payload string) (bool, error) {
	f.t.Helper()
	f.events++
	f.fake.sub = live
	if payload == "" {
		payload = fmt.Sprintf(`"id":%q,"object":"subscription"`, live.ID)
	}
	event, err := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":%q,%s}`, fmt.Sprintf("evt_grant_%s_%d", f.x, f.events), eventType, `"created":`+fmt.Sprint(time.Now().Unix())+`,"data":{"object":{`+payload+`}}`)))
	if err != nil {
		f.t.Fatal(err)
	}
	return f.service.HandleWebhook(f.ctx, event)
}

func (f *grantFixture) replay(live billing.StripeSubscription) {
	f.t.Helper()
	if processed, err := f.deliver(live, "customer.subscription.updated", ""); err != nil || !processed {
		f.t.Fatalf("processed=%t err=%v", processed, err)
	}
}

func (f *grantFixture) recordedEvents() int {
	return f.storeQueryCount(`SELECT count(*) FROM paperboat.stripe_events WHERE id LIKE $1`, "evt_grant_"+f.x+"%")
}

// A support plan grant owns the workspace's plan while it runs (grant_until): a Stripe snapshot that is not a live paid
// subscription is not applied, however old the event carrying it, while a subscription the customer really pays for is
// applied and ends the grant.
func TestPlanGrantIsNotOverwrittenByAnOldSubscriptionsEvent(t *testing.T) {
	f := newGrantFixture(t)
	// Control: with no grant, the old subscription's snapshot is accepted by every checkout guard and rewrites the row.
	f.exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id=NULL,state='free' WHERE id=$1`, f.workspace)
	f.replay(f.old("canceled"))
	if sub, _, state, _ := f.row(); sub != "sub_old_"+f.x || state != "canceled" {
		t.Fatalf("control: the old subscription was not applied to a row with no grant: %s %s", sub, state)
	}

	f.grant(10)
	// A webhook right after the grant, and replays of any age and in any non-live state, change nothing.
	for _, status := range []string{"canceled", "incomplete_expired", "incomplete", "unpaid", "past_due", "paused"} {
		f.replay(f.old(status))
		f.granted("replay of " + status)
	}
	f.exec(`UPDATE paperboat.billing_workspaces SET period_start=now()-interval '9 days',updated_at=now()-interval '9 days' WHERE id=$1`, f.workspace)
	f.replay(f.old("canceled"))
	f.granted("replay long after the grant")
	if n := f.recordedEvents(); n < 8 {
		t.Fatalf("ignored events must be recorded so Stripe stops retrying them: %d", n)
	}

	// The customer really subscribes (a newer checkout, no predecessor): the subscription is applied and the grant ends.
	f.checkoutOp("checkout-new-"+f.x, "")
	f.replay(f.fresh)
	if sub, _, state, running := f.row(); sub != "sub_new_"+f.x || state != "active" || running {
		t.Fatalf("a paid subscription must replace the grant: sub=%q state=%q grant running=%t", sub, state, running)
	}

	// A checkout started before the grant (it names the lapsed subscription as predecessor, an id the grant cleared) still
	// applies when its subscription turns active: the customer paid, and a grant must not swallow that.
	f.exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id='sub_lapsed_'||$2,state='canceled' WHERE id=$1`, f.workspace, f.x)
	f.checkoutOp("checkout-before-"+f.x, "sub_lapsed_"+f.x)
	f.grant(10)
	before := f.fresh
	before.ID, before.ItemID, before.CheckoutOperation = "sub_before_"+f.x, "si_before_"+f.x, "checkout-before-"+f.x
	f.replay(before)
	if sub, _, state, running := f.row(); sub != "sub_before_"+f.x || state != "active" || running {
		t.Fatalf("a checkout started before the grant must still apply: sub=%q state=%q running=%t", sub, state, running)
	}

	// An expired grant (both its end and its period are past) protects nothing, and the lapsed snapshot is applied.
	f.exec(`UPDATE paperboat.billing_workspaces SET provider_subscription_id=NULL,state='active',period_end=now()-interval '1 second',grant_until=now()-interval '1 second' WHERE id=$1`, f.workspace)
	f.replay(f.old("canceled"))
	if sub, _, state, running := f.row(); sub != "sub_old_"+f.x || state != "canceled" || running {
		t.Fatalf("expired grant: sub=%q state=%q running=%t", sub, state, running)
	}
}

// One checkout's subscription is first reported incomplete (payment not yet confirmed) and then active: the first is
// ignored (acknowledged, so Stripe stops retrying it) and leaves the grant, the second applies and ends it.
func TestPlanGrantSurvivesAnIncompleteCheckoutUntilItIsPaid(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(10)
	f.checkoutOp("checkout-new-"+f.x, "")
	incomplete := f.fresh
	incomplete.Status = "incomplete"
	f.replay(incomplete)
	f.granted("incomplete")
	f.replay(f.fresh) // the same subscription, now active
	if sub, _, state, running := f.row(); sub != "sub_new_"+f.x || state != "active" || running {
		t.Fatalf("the paid subscription must replace the grant: sub=%q state=%q running=%t", sub, state, running)
	}
}

// The webhook payload is never trusted: the subscription is read from Stripe when the event is processed. A payload
// that says "active" for a subscription Stripe now reports cancelled changes nothing; a payload that says "deleted" for a
// subscription Stripe reports active and paid applies it; a payload reporting a cancellation Stripe's read does not show
// yet (a stale read) is retried, not acknowledged, and touches nothing.
func TestPlanGrantFollowsStripesLiveReadNotTheEventPayload(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(10)
	processed, err := f.deliver(f.old("canceled"), "customer.subscription.updated", fmt.Sprintf(`"id":%q,"object":"subscription","status":"active"`, "sub_old_"+f.x))
	if err != nil || !processed {
		t.Fatalf("payload active, live cancelled: processed=%t err=%v", processed, err)
	}
	f.granted("payload says active, live says cancelled")

	live := f.fresh
	f.checkoutOp("checkout-new-"+f.x, "")
	staleCancel := fmt.Sprintf(`"id":%q,"object":"subscription","cancel_at_period_end":true`, live.ID)
	processed, err = f.deliver(live, "customer.subscription.updated", staleCancel)
	if !errors.Is(err, billing.ErrProviderOutcomeUnknown) || processed {
		t.Fatalf("a cancellation the live read does not show yet must be retried: processed=%t err=%v", processed, err)
	}
	f.granted("stale cancellation read")
	before := f.recordedEvents()

	processed, err = f.deliver(live, "customer.subscription.deleted", fmt.Sprintf(`"id":%q,"object":"subscription","status":"canceled"`, live.ID))
	if err != nil || !processed {
		t.Fatalf("payload cancelled, live active: processed=%t err=%v", processed, err)
	}
	if sub, _, state, running := f.row(); sub != live.ID || state != "active" || running {
		t.Fatalf("the live paid subscription must apply whatever the payload says: sub=%q state=%q running=%t", sub, state, running)
	}
	if f.recordedEvents() != before+1 {
		t.Fatalf("exactly the applied event must have been recorded: %d -> %d", before, f.recordedEvents())
	}
}

// An old subscription that Stripe reports live-active again was never really lapsed (it is paid for): its snapshot is
// applied and ends the grant, but only if the checkout that created it vouches for it. Reported cancelled it changes nothing.
func TestPlanGrantAndAnOldSubscriptionThatIsActiveAgain(t *testing.T) {
	f := newGrantFixture(t)
	f.grant(10)
	f.replay(f.old("canceled"))
	f.granted("old subscription cancelled")
	f.replay(f.old("active"))
	if sub, _, state, running := f.row(); sub != "sub_old_"+f.x || state != "active" || running {
		t.Fatalf("an old subscription that is live again must be applied: sub=%q state=%q running=%t", sub, state, running)
	}
}

// The customer's own billing answer says a running grant is one, and until when; an expired grant is not one.
func TestWorkspaceOverviewSaysWhenAPlanIsAGrant(t *testing.T) {
	f := newGrantFixture(t)
	overview := func() billing.Workspace {
		t.Helper()
		w, err := f.service.Overview(f.ctx, f.user, "")
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if w := overview(); w.Granted || w.GrantedUntil != nil {
		t.Fatalf("a lapsed Stripe plan reads as granted: %+v", w.GrantedUntil)
	}
	f.grant(10)
	if w := overview(); !w.Granted || w.GrantedUntil == nil || time.Until(*w.GrantedUntil) < 9*24*time.Hour || w.ProductCode != f.code {
		t.Fatalf("a running grant: granted=%t until=%v product=%q", w.Granted, w.GrantedUntil, w.ProductCode)
	}
	f.exec(`UPDATE paperboat.billing_workspaces SET period_start=now()-interval '2 seconds',period_end=now()-interval '1 second',grant_until=now()-interval '1 second' WHERE id=$1`, f.workspace)
	if w := overview(); w.Granted || w.GrantedUntil != nil {
		t.Fatalf("an expired grant reads as granted: %v", w.GrantedUntil)
	}
	f.grant(10)
	f.checkoutOp("checkout-new-"+f.x, "")
	f.replay(f.fresh)
	if w := overview(); w.Granted {
		t.Fatal("a paid subscription still reads as granted")
	}
}
