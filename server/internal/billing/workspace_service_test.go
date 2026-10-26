package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type workspaceStripeFake struct {
	customers, checkouts, portals, gets        int
	sub                                        billing.StripeSubscription
	checkoutTimeout, portalTimeout             bool
	checkoutInputs                             []billing.StripeCheckoutInput
	portalCustomers, portalReturns, portalKeys []string
	portalConfigurations                       []string
	checkoutState                              string
	// lockProbe, when set, runs inside each provider read: it fails if the caller holds a database lock.
	lockProbe    func() error
	lockProbeErr error
}

func (f *workspaceStripeFake) CreateCustomer(_ context.Context, workspaceID, email, key string) (string, error) {
	f.customers++
	return "cus_" + workspaceID, nil
}
func (f *workspaceStripeFake) CreateSubscriptionCheckout(_ context.Context, in billing.StripeCheckoutInput) (billing.CheckoutSession, error) {
	f.checkouts++
	f.checkoutInputs = append(f.checkoutInputs, in)
	if f.checkoutTimeout {
		f.checkoutTimeout = false
		return billing.CheckoutSession{}, context.DeadlineExceeded
	}
	return billing.CheckoutSession{URL: "https://checkout.stripe.test/session", ProviderSessionID: "cs_test", ProviderCustomerID: in.CustomerID}, nil
}
func (f *workspaceStripeFake) CreatePortal(_ context.Context, in billing.StripePortalInput) (billing.CustomerPortalSession, error) {
	f.portals++
	f.portalConfigurations = append(f.portalConfigurations, in.ConfigurationID)
	f.portalCustomers = append(f.portalCustomers, in.CustomerID)
	f.portalReturns = append(f.portalReturns, in.ReturnURL)
	f.portalKeys = append(f.portalKeys, in.IdempotencyKey)
	if f.portalTimeout {
		f.portalTimeout = false
		return billing.CustomerPortalSession{}, context.DeadlineExceeded
	}
	return billing.CustomerPortalSession{URL: "https://billing.stripe.test/session"}, nil
}
func (f *workspaceStripeFake) GetCheckoutSession(_ context.Context, id string) (billing.StripeCheckoutState, error) {
	if f.lockProbe != nil && f.lockProbeErr == nil {
		f.lockProbeErr = f.lockProbe()
	}
	in := f.checkoutInputs[len(f.checkoutInputs)-1]
	status := f.checkoutState
	if status == "" {
		status = "open"
	}
	return billing.StripeCheckoutState{ID: id, CustomerID: in.CustomerID, CheckoutOperation: in.IdempotencyKey, Status: status, URL: "https://checkout.stripe.test/session"}, nil
}

func TestWorkspaceCheckoutAndPortalRecoverSameProviderOperation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, code := "usr_recover_"+x, "recover-plan-"+x
	insertUser(t, store, user, user+"@example.test")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'personal_subscription','{}',true)`, "bp_recover_"+x, code, "prod_"+x, "price_"+x); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	f := &workspaceStripeFake{checkoutTimeout: true}
	s := billing.NewWorkspaceService(store, f, "recovery-key", billing.PortalConfigurations{Personal: "bpc_personal_original", Team: "bpc_team_original"})
	key := "checkout-recover-" + x
	if _, err := s.Checkout(ctx, user, user+"@example.test", "", code, key, "https://example.test/success", "https://example.test/cancel", 1); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatalf("first checkout error=%v", err)
	}
	result, err := s.Checkout(ctx, user, user+"@example.test", "", code, key, "https://example.test/success", "https://example.test/cancel", 1)
	if err != nil || result.ProviderSessionID != "cs_test" || f.customers != 1 || f.checkouts != 2 {
		t.Fatalf("recovered checkout=%+v calls=%d/%d err=%v", result, f.customers, f.checkouts, err)
	}
	if len(f.checkoutInputs) != 2 || f.checkoutInputs[0] != f.checkoutInputs[1] || f.checkoutInputs[0].IdempotencyKey != key || f.checkoutInputs[0].CustomerID != "cus_personal:"+user || f.checkoutInputs[0].PriceID != "price_"+x || f.checkoutInputs[0].Quantity != 1 {
		t.Fatalf("checkout inputs=%+v", f.checkoutInputs)
	}
	f.portalTimeout = true
	portalKey := "portal-recover-" + x
	if _, err = s.Portal(ctx, user, "", portalKey, "https://example.test/account"); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatalf("first portal error=%v", err)
	}
	// A restart with new portal configuration must recover the exact original
	// provider request rather than changing an in-flight operation's scope.
	s = billing.NewWorkspaceService(store, f, "recovery-key", billing.PortalConfigurations{Personal: "bpc_personal_next", Team: "bpc_team_next"})
	portal, err := s.Portal(ctx, user, "", portalKey, "https://example.test/account")
	if err != nil || portal.URL == "" || f.portals != 2 {
		t.Fatalf("recovered portal=%+v calls=%d err=%v", portal, f.portals, err)
	}
	if f.portalCustomers[0] != f.portalCustomers[1] || f.portalReturns[0] != f.portalReturns[1] || f.portalKeys[0] != portalKey || f.portalKeys[1] != portalKey {
		t.Fatalf("portal calls customers=%v returns=%v keys=%v", f.portalCustomers, f.portalReturns, f.portalKeys)
	}
	if f.portalConfigurations[0] != "bpc_personal_original" || f.portalConfigurations[1] != "bpc_personal_original" {
		t.Fatal("portal recovery changed its original configuration")
	}
	team := "portal_team_" + x
	teamWorkspace := "team:" + team
	t.Cleanup(func() {
		store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, teamWorkspace)
		store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, teamWorkspace)
		store.SQL().ExecContext(ctx, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
	})
	for _, q := range []string{`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`} {
		if _, err = store.SQL().ExecContext(ctx, q, team, user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,provider_customer_id) VALUES($1,$2,$3)`, teamWorkspace, team, "cus_team_"+x); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Portal(ctx, user, team, "team-portal-"+x, "https://example.test/team"); err != nil {
		t.Fatal(err)
	}
	last := len(f.portalConfigurations) - 1
	if f.portalConfigurations[last] != "bpc_team_next" || f.portalCustomers[last] != "cus_team_"+x {
		t.Fatal("team portal used a personal configuration or customer")
	}
	staleKey := "stale-pending-" + x
	f.portalTimeout = true
	if _, err = s.Portal(ctx, user, "", staleKey, "https://example.test/stale"); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET state='pending',provider_attempted_at=NULL,updated_at=now()-interval '2 minutes' WHERE idempotency_key=$1`, staleKey); err != nil {
		t.Fatal(err)
	}
	beforeStale := f.portals
	if _, err = s.Portal(ctx, user, "", staleKey, "https://example.test/stale"); err != nil || f.portals != beforeStale+1 {
		t.Fatalf("stale pending calls=%d err=%v", f.portals-beforeStale, err)
	}
	expiredKey := "expired-" + x
	f.portalTimeout = true
	if _, err = s.Portal(ctx, user, "", expiredKey, "https://example.test/expired"); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET provider_attempted_at=now()-interval '25 hours' WHERE idempotency_key=$1`, expiredKey); err != nil {
		t.Fatal(err)
	}
	before := f.portals
	if _, err = s.Portal(ctx, user, "", expiredKey, "https://example.test/expired"); !errors.Is(err, billing.ErrStripeOperationExpired) || f.portals != before {
		t.Fatalf("expired portal calls=%d err=%v", f.portals-before, err)
	}
	f.checkoutState = "expired"
	beforeCheckout := f.checkouts
	if _, err = s.Checkout(ctx, user, user+"@example.test", "", code, "after-expired-"+x, "https://example.test/new-success", "https://example.test/new-cancel", 1); err != nil || f.checkouts != beforeCheckout+1 {
		t.Fatalf("new checkout after expired session calls=%d err=%v", f.checkouts-beforeCheckout, err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET provider_subscription_id=$2,state='canceled',updated_at=now() WHERE id=$1`, "personal:"+user, "sub_canceled_"+x); err != nil {
		t.Fatal(err)
	}
	f.checkoutState = "open"
	if _, err = s.Checkout(ctx, user, user+"@example.test", "", code, "replacement-"+x, "https://example.test/success", "https://example.test/cancel", 1); err != nil {
		t.Fatal(err)
	}
	beforeCheckout = f.checkouts
	if _, err = s.Checkout(ctx, user, user+"@example.test", "", code, "duplicate-replacement-"+x, "https://example.test/success", "https://example.test/cancel", 1); !errors.Is(err, billing.ErrCheckoutPending) || f.checkouts != beforeCheckout {
		t.Fatalf("duplicate replacement calls=%d err=%v", f.checkouts-beforeCheckout, err)
	}
}
func (f *workspaceStripeFake) GetSubscription(context.Context, string) (billing.StripeSubscription, error) {
	if f.lockProbe != nil && f.lockProbeErr == nil {
		f.lockProbeErr = f.lockProbe()
	}
	f.gets++
	return f.sub, nil
}
func (f *workspaceStripeFake) UpdateSubscriptionSeats(context.Context, string, string, int, string) (billing.StripeSubscription, error) {
	return billing.StripeSubscription{}, nil
}

func TestWorkspaceServicePersonalCheckoutReplayScopeAndWebhook(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "usr_ws_" + suffix
	code := "personal-pro-" + suffix
	price := "price_" + suffix
	insertUser(t, store, user, "workspace-"+suffix+"@example.com")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'personal_subscription',$5,true)`, "bp_"+suffix, code, "prod_"+suffix, price, `{"display_name":"Paperboat Pro","allowance_bytes":4096}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id=$1`, "evt_"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id=$1`, "evt_stale_"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id=$1`, "evt_interval_"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id=$1`, "evt_cancel_"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id LIKE $1`, "evt_replacement_%"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.stripe_events WHERE id=$1`, "evt_repeat_old_"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+user)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	fake := &workspaceStripeFake{}
	service := billing.NewWorkspaceService(store, fake, "workspace-test-key")
	products, err := service.Products(ctx, user, "", 200, 0)
	foundProduct := false
	for _, p := range products.Items {
		if p.Code == code && p.Name == "Paperboat Pro" {
			foundProduct = true
		}
	}
	if err != nil || !foundProduct {
		t.Fatalf("products=%+v err=%v", products, err)
	}
	in := []string{user, "workspace@example.com", "", code, "checkout-" + suffix, "https://paperboat.test/success", "https://paperboat.test/cancel"}
	first, err := service.Checkout(ctx, in[0], in[1], in[2], in[3], in[4], in[5], in[6], 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Checkout(ctx, in[0], in[1], in[2], in[3], in[4], in[5], in[6], 1)
	if err != nil || second != first {
		t.Fatalf("replay=%+v err=%v", second, err)
	}
	if fake.customers != 1 || fake.checkouts != 1 {
		t.Fatalf("provider calls customer/checkout=%d/%d", fake.customers, fake.checkouts)
	}
	// The Stripe read for the earlier checkout runs with no database lock held (F21-20): another
	// transaction takes the team write lock meanwhile.
	fake.lockProbe = func() error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
				return err
			}
			return db.LockTeamWrites(ctx, tx)
		})
	}
	if _, err = service.Checkout(ctx, user, "workspace@example.com", "", code, "second-"+suffix, in[5], in[6], 1); err != billing.ErrCheckoutPending {
		t.Fatalf("second checkout error=%v", err)
	}
	if fake.lockProbe = nil; fake.lockProbeErr != nil {
		t.Fatalf("the Stripe read ran while the team write lock was held: %v", fake.lockProbeErr)
	}
	wrongCode := "team-plan-" + suffix
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'team_subscription','team',true)`, "bp_team_"+suffix, wrongCode, "prod_team_"+suffix, "price_team_"+suffix); err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code=$1`, wrongCode)
	if _, err = service.Checkout(ctx, user, "workspace@example.com", "", wrongCode, "wrong-"+suffix, in[5], in[6], 1); err == nil {
		t.Fatal("team product accepted for personal workspace")
	}
	fake.sub = billing.StripeSubscription{ID: "sub_" + suffix, CustomerID: first.ProviderCustomerID, PriceID: price, ItemID: "si_" + suffix, Status: "active", CheckoutOperation: in[4], Quantity: 1, PeriodStart: time.Unix(1700000000, 0).UTC(), PeriodEnd: time.Unix(1702592000, 0).UTC()}
	event, err := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":"customer.subscription.updated","data":{"object":{"id":%q,"object":"subscription"}}}`, "evt_"+suffix, fake.sub.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET state='uncertain' WHERE idempotency_key=$1`, in[4]); err != nil {
		t.Fatal(err)
	}
	processed, err := service.HandleWebhook(ctx, event)
	if !errors.Is(err, billing.ErrProviderOutcomeUnknown) || processed {
		t.Fatalf("uncertain checkout webhook processed=%v err=%v", processed, err)
	}
	var eventRecorded bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.stripe_events WHERE id=$1)`, "evt_"+suffix).Scan(&eventRecorded); err != nil || eventRecorded {
		t.Fatalf("uncertain checkout event recorded=%v err=%v", eventRecorded, err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspace_operations SET state='succeeded' WHERE idempotency_key=$1`, in[4]); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_products SET active=false WHERE code=$1`, code); err != nil {
		t.Fatal(err)
	}
	// The subscription read runs with no lock on the workspace row (F21-20).
	fake.lockProbe = func() error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var id string
			return tx.QueryRow(ctx, `SELECT id FROM paperboat.billing_workspaces WHERE id=$1 FOR UPDATE NOWAIT`, "personal:"+user).Scan(&id)
		})
	}
	processed, err = service.HandleWebhook(ctx, event)
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	if fake.lockProbe = nil; fake.lockProbeErr != nil {
		t.Fatalf("the subscription read ran while the workspace row was locked: %v", fake.lockProbeErr)
	}
	processed, err = service.HandleWebhook(ctx, event)
	if err != nil || processed {
		t.Fatalf("duplicate processed=%v err=%v", processed, err)
	}
	w, err := service.Overview(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.SubscriptionID != fake.sub.ID || w.Quantity != 1 || w.ProductCode != code || w.State != "active" {
		t.Fatalf("workspace=%+v", w)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_products SET active=true WHERE code=$1`, code); err != nil {
		t.Fatal(err)
	}
	beforeCheckout := fake.checkouts
	if _, err = service.Checkout(ctx, user, "workspace@example.com", "", code, "duplicate-active-"+suffix, in[5], in[6], 1); !errors.Is(err, billing.ErrWorkspaceSubscriptionExists) || fake.checkouts != beforeCheckout {
		t.Fatalf("active duplicate calls=%d err=%v", fake.checkouts-beforeCheckout, err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET state='past_due' WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Checkout(ctx, user, "workspace@example.com", "", code, "duplicate-due-"+suffix, in[5], in[6], 1); !errors.Is(err, billing.ErrWorkspaceSubscriptionExists) || fake.checkouts != beforeCheckout {
		t.Fatalf("past-due duplicate calls=%d err=%v", fake.checkouts-beforeCheckout, err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET state='active' WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	// Capture the original paid clock when the provider snapshot is accepted,
	// before any usage is reported, and retain it on subsequent billing changes.
	originalStart := fake.sub.PeriodStart
	fake.sub.PeriodStart = originalStart.Add(12 * time.Hour)
	fake.sub.PeriodEnd = fake.sub.PeriodStart.AddDate(1, 0, 0)
	intervalEvent := event
	intervalEvent.ID = "evt_interval_" + suffix
	if processed, err = service.HandleWebhook(ctx, intervalEvent); err != nil || !processed {
		t.Fatal("interval webhook failed", err)
	}
	var anchor time.Time
	if err = store.SQL().QueryRowContext(ctx, `SELECT bandwidth_anchor FROM paperboat.billing_workspaces WHERE id=$1`, w.ID).Scan(&anchor); err != nil || !anchor.Equal(originalStart) {
		t.Fatal("provider update moved the original bandwidth anchor", err)
	}
	// A portal cancellation arrives while Stripe still serves the pre-cancel
	// subscription: the event must be retried, not acknowledged, and once the
	// provider read catches up the cancellation clears the dead seat reduction.
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET scheduled_quantity=quantity,scheduled_quantity_at=period_end WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	cancelEvent, err := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":"customer.subscription.updated","created":%d,"data":{"object":{"id":%q,"object":"subscription","cancel_at_period_end":false,"cancel_at":%d,"items":{"data":[{"current_period_end":%d}]}}}}`, "evt_cancel_"+suffix, time.Now().Unix(), fake.sub.ID, fake.sub.PeriodEnd.Unix(), fake.sub.PeriodEnd.Unix())))
	if err != nil {
		t.Fatal(err)
	}
	if processed, err = service.HandleWebhook(ctx, cancelEvent); !errors.Is(err, billing.ErrProviderOutcomeUnknown) || processed {
		t.Fatalf("stale provider read acknowledged a cancellation: processed=%v err=%v", processed, err)
	}
	fake.sub.CancelAtPeriodEnd = true
	if processed, err = service.HandleWebhook(ctx, cancelEvent); err != nil || !processed {
		t.Fatalf("cancellation retry processed=%v err=%v", processed, err)
	}
	var scheduled sql.NullInt64
	if w, err = service.Overview(ctx, user, ""); err != nil || !w.CancelAtPeriodEnd {
		t.Fatalf("portal cancellation not reflected: %+v err=%v", w, err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT scheduled_quantity FROM paperboat.billing_workspaces WHERE id=$1`, w.ID).Scan(&scheduled); err != nil || scheduled.Valid {
		t.Fatalf("cancelling subscription kept a scheduled seat reduction: %v err=%v", scheduled, err)
	}
	fake.sub.CancelAtPeriodEnd = false
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET cancel_at_period_end=false WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	old := fake.sub
	old.ID = "sub_old_" + suffix
	old.ItemID = "si_old_" + suffix
	old.Status = "canceled"
	fake.sub = old
	stale, err := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":"customer.subscription.deleted","data":{"object":{"id":%q,"object":"subscription"}}}`, "evt_stale_"+suffix, old.ID)))
	if err != nil {
		t.Fatal(err)
	}
	processed, err = service.HandleWebhook(ctx, stale)
	if err != nil || !processed {
		t.Fatalf("stale processed=%v err=%v", processed, err)
	}
	w, err = service.Overview(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.SubscriptionID != "sub_"+suffix || w.State != "active" {
		t.Fatalf("stale event overwrote workspace: %+v", w)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET state='canceled',updated_at=now()-interval '1 second' WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	replacementKey := "replacement-" + suffix
	if _, err = service.Checkout(ctx, user, "workspace@example.com", "", code, replacementKey, in[5], in[6], 1); err != nil {
		t.Fatal(err)
	}
	repeatedOld := billing.StripeSubscription{ID: "sub_" + suffix, CustomerID: first.ProviderCustomerID, PriceID: price, ItemID: "si_" + suffix, Status: "canceled", CheckoutOperation: in[4], Quantity: 1, PeriodStart: fake.sub.PeriodStart, PeriodEnd: fake.sub.PeriodEnd}
	fake.sub = repeatedOld
	repeatedEvent, parseErr := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":"customer.subscription.deleted","data":{"object":{"id":%q,"object":"subscription"}}}`, "evt_repeat_old_"+suffix, repeatedOld.ID)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if processed, err = service.HandleWebhook(ctx, repeatedEvent); err != nil || !processed {
		t.Fatalf("repeat old processed=%v err=%v", processed, err)
	}
	beforeReplacement := fake.checkouts
	if _, err = service.Checkout(ctx, user, "workspace@example.com", "", code, "duplicate-after-old-"+suffix, in[5], in[6], 1); !errors.Is(err, billing.ErrCheckoutPending) || fake.checkouts != beforeReplacement {
		t.Fatalf("old event reopened checkout calls=%d err=%v", fake.checkouts-beforeReplacement, err)
	}
	for _, tc := range []struct {
		name, operation string
		accept          bool
	}{{"missing", "", false}, {"wrong", "not-the-operation", false}, {"exact", replacementKey, true}} {
		fake.sub = billing.StripeSubscription{ID: "sub_replacement_" + tc.name + suffix, CustomerID: first.ProviderCustomerID, PriceID: price, ItemID: "si_replacement_" + tc.name + suffix, Status: "active", CheckoutOperation: tc.operation, Quantity: 1, PeriodStart: fake.sub.PeriodStart, PeriodEnd: fake.sub.PeriodEnd}
		e, parseErr := billing.ParseStripeWebhook([]byte(fmt.Sprintf(`{"id":%q,"type":"customer.subscription.updated","data":{"object":{"id":%q,"object":"subscription"}}}`, "evt_replacement_"+tc.name+suffix, fake.sub.ID)))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if processed, err = service.HandleWebhook(ctx, e); err != nil || !processed {
			t.Fatalf("replacement %s processed=%v err=%v", tc.name, processed, err)
		}
		w, err = service.Overview(ctx, user, "")
		if err != nil {
			t.Fatal(err)
		}
		got := w.SubscriptionID == fake.sub.ID
		if got != tc.accept {
			t.Fatalf("replacement %s accepted=%v workspace=%+v", tc.name, got, w)
		}
	}
}

func TestWorkspaceServiceNilProviderIsExplicit(t *testing.T) {
	s := billing.NewWorkspaceService(nil, nil, "")
	if _, err := s.Checkout(context.Background(), "u", "e", "", "p", "k", "s", "c", 1); err != billing.ErrStripeUnavailable {
		t.Fatalf("checkout error=%v", err)
	}
	if _, err := s.Portal(context.Background(), "u", "", "k", "r"); err != billing.ErrStripeUnavailable {
		t.Fatalf("portal error=%v", err)
	}
}
