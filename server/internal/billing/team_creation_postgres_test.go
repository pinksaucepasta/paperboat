package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

// This exercises the real HTTP provider adapter and PostgreSQL transaction owner:
// neither subscription creation nor an unpaid Checkout is authority to create a Team.
func TestPaidTeamCreationRequiresAuthoritativePayment(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	actor, team, key := "usr_paid_team_"+suffix, "paid-team-"+suffix, "paid-team-op-"+suffix
	insertUser(t, store, actor, actor+"@example.test")
	customer, session, subscription := "cus_"+suffix, "cs_"+suffix, "sub_"+suffix
	price := "price_team_" + suffix
	_, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,'team-month','stripe',$2,$3,'team_subscription','{}',true,1500,'usd','month')`, "bp_"+suffix, "prod_"+suffix, price)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.stripe_events WHERE subscription_id=$1`,
			`DELETE FROM paperboat.billing_workspaces WHERE provider_subscription_id=$1`,
		} {
			store.SQL().ExecContext(context.Background(), q, subscription)
		}
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_team_orders WHERE actor_user_id=$1`, actor)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.audit_events WHERE actor_user_id=$1`, actor)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, actor)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE id=$1`, "bp_"+suffix)
	})
	paid, status, quantity := "unpaid", "open", 3
	checkoutCustomer, checkoutOperation, checkoutMode, checkoutPrice := customer, "team-create:"+key, "subscription", price
	checkoutLive := false
	failCheckout := true
	failCode := 500
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/customers":
			json.NewEncoder(w).Encode(map[string]any{"id": customer})
		case r.Method == "POST" && r.URL.Path == "/v1/checkout/sessions":
			r.ParseForm()
			if r.Form.Get("line_items[0][price]") != price || r.Form.Get("line_items[0][quantity]") != "3" || r.Form.Get("metadata[paperboat_checkout_operation]") != "team-create:"+key {
				t.Error("checkout did not use immutable catalog/seat/operation request")
			}
			if failCheckout {
				failCheckout = false
				w.WriteHeader(failCode)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": session, "customer": customer, "url": "https://checkout.example.test/session"})
		case r.Method == "GET" && r.URL.Path == "/v1/checkout/sessions/"+session:
			json.NewEncoder(w).Encode(map[string]any{"id": session, "customer": checkoutCustomer, "status": status, "mode": checkoutMode, "livemode": checkoutLive, "payment_status": paid, "subscription": subscription, "metadata": map[string]string{"paperboat_checkout_operation": checkoutOperation}, "line_items": map[string]any{"data": []any{map[string]any{"quantity": quantity, "price": map[string]string{"id": checkoutPrice}}}}})
		case r.Method == "GET" && r.URL.Path == "/v1/subscriptions/"+subscription:
			json.NewEncoder(w).Encode(map[string]any{"id": subscription, "customer": customer, "status": "active", "metadata": map[string]string{"paperboat_checkout_operation": "team-create:" + key}, "items": map[string]any{"data": []any{map[string]any{"id": "si_" + suffix, "quantity": 3, "current_period_start": time.Now().Unix() - 60, "current_period_end": time.Now().Unix() + 86400, "price": map[string]string{"id": price}}}}})
		default:
			t.Errorf("unexpected provider method/path")
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	s := billing.NewWorkspaceService(store, billing.HTTPStripeClient{BaseURL: provider.URL, APIKey: "sk_test_fixture", Client: provider.Client()}, "paid-team-encryption-key")
	in := billing.TeamCreationInput{OperationID: key, TeamID: team, Seats: 3, Interval: "month", SuccessURL: "https://console.example.test/team?created=" + team, CancelURL: "https://console.example.test/team"}
	assertAbsent := func() {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.teams WHERE team_id=$1)+(SELECT count(*) FROM paperboat.team_members WHERE team_id=$1)+(SELECT count(*) FROM paperboat.billing_workspaces WHERE team_id=$1)`, team).Scan(&n); err != nil || n != 0 {
			t.Fatalf("unpaid Team/access/workspace exists: count=%d err=%v", n, err)
		}
	}
	if _, err = s.CreatePaidTeam(ctx, actor, actor+"@example.test", in); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
		t.Fatalf("uncertain checkout error=%v", err)
	}
	assertAbsent()
	result, err := s.CreatePaidTeam(ctx, actor, actor+"@example.test", in)
	if err != nil || result.URL == "" {
		t.Fatalf("recover same checkout: %v", err)
	}
	assertAbsent()
	replay, err := s.CreatePaidTeam(ctx, actor, actor+"@example.test", in)
	if err != nil || replay != result {
		t.Fatalf("checkout replay: %v", err)
	}
	other := in
	other.OperationID += "-other"
	if _, err = s.CreatePaidTeam(ctx, actor, "", other); !errors.Is(err, billing.ErrTeamReserved) {
		t.Fatalf("duplicate slug: %v", err)
	}
	other = in
	other.Seats++
	if _, err = s.CreatePaidTeam(ctx, actor, "", other); !errors.Is(err, billing.ErrIdempotencyConflict) {
		t.Fatalf("changed request: %v", err)
	}
	event := billing.StripeWebhook{ID: "evt_early_" + suffix, Type: "customer.subscription.created"}
	event.Data.Object.Object = "subscription"
	event.Data.Object.ID = subscription
	if _, err = s.HandleWebhook(ctx, event); !errors.Is(err, billing.ErrCheckoutPending) {
		t.Fatalf("early subscription: %v", err)
	}
	assertAbsent()
	var count int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.stripe_events WHERE id=$1`, event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("early event was acknowledged/deduplicated")
	}
	event.ID = "evt_checkout_" + suffix
	event.Type = "checkout.session.completed"
	event.Data.Object.Object = "checkout.session"
	event.Data.Object.ID = session
	status = "complete"
	if _, err = s.HandleWebhook(ctx, event); !errors.Is(err, billing.ErrCheckoutPending) {
		t.Fatalf("completed unpaid: %v", err)
	}
	assertAbsent()
	paid = "paid"
	for _, tc := range []struct {
		name    string
		change  func()
		restore func()
	}{
		{"quantity", func() { quantity = 4 }, func() { quantity = 3 }},
		{"customer", func() { checkoutCustomer = "cus_foreign" }, func() { checkoutCustomer = customer }},
		{"operation", func() { checkoutOperation = "team-create:foreign" }, func() { checkoutOperation = "team-create:" + key }},
		{"mode", func() { checkoutMode = "payment" }, func() { checkoutMode = "subscription" }},
		{"price", func() { checkoutPrice = "price_foreign" }, func() { checkoutPrice = price }},
		{"live mode", func() { checkoutLive = true }, func() { checkoutLive = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			defer tc.restore()
			if _, err = s.HandleWebhook(ctx, event); err == nil {
				t.Fatal("unverified paid callback accepted")
			}
			assertAbsent()
		})
	}
	// Stripe can complete before the HTTP caller persists its Checkout response.
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_team_orders SET state='pending',provider_session_id=NULL,result_ciphertext=NULL WHERE operation_id=$1`, key); err != nil {
		t.Fatal(err)
	}
	if processed, err := s.HandleWebhook(ctx, event); err != nil || !processed {
		t.Fatalf("paid fulfillment: processed=%v err=%v", processed, err)
	}
	var owner string
	var seats, members int
	if err = store.SQL().QueryRowContext(ctx, `SELECT t.owner_account,w.quantity,(SELECT count(*) FROM paperboat.team_members m WHERE m.team_id=t.team_id AND m.account_id=t.owner_account AND m.active) FROM paperboat.teams t JOIN paperboat.billing_workspaces w USING(team_id) WHERE t.team_id=$1 AND w.provider_subscription_id=$2 AND w.provider_customer_id=$3`, team, subscription, customer).Scan(&owner, &seats, &members); err != nil || owner != actor || seats != 3 || members != 1 {
		t.Fatalf("atomic materialization owner/seats/member=%s/%d/%d err=%v", owner, seats, members, err)
	}
	// A subscription event that arrived too early converges after paid materialization.
	event.ID = "evt_early_" + suffix
	event.Type = "customer.subscription.created"
	event.Data.Object.Object = "subscription"
	event.Data.Object.ID = subscription
	if _, err = s.HandleWebhook(ctx, event); err != nil {
		t.Fatalf("early event retry: %v", err)
	}
	if _, err = s.HandleWebhook(ctx, event); err != nil {
		t.Fatalf("duplicate paid callback: %v", err)
	}
	// Authoritative expiration releases the reservation without granting access.
	key += "-expired"
	in.OperationID = key
	in.TeamID += "-expired"
	customer += "-expired"
	session += "-expired"
	status = "expired"
	checkoutCustomer = customer
	checkoutOperation = "team-create:" + key
	if _, err = s.CreatePaidTeam(ctx, actor, "", in); err != nil {
		t.Fatalf("create expiring checkout: %v", err)
	}
	event.ID += "-expired"
	event.Data.Object.Object = "checkout.session"
	event.Data.Object.ID = session
	event.Type = "checkout.session.expired"
	if _, err = s.HandleWebhook(ctx, event); err != nil {
		t.Fatalf("expired callback: %v", err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.teams WHERE team_id=$1`, in.TeamID).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired checkout created Team access")
	}
	// A definitive rejection keeps a terminal receipt while freeing the slug.
	key += "-failed"
	in.OperationID = key

	customer += "-failed"
	failCheckout = true
	failCode = 400
	if _, err = s.CreatePaidTeam(ctx, actor, "", in); !errors.Is(err, billing.ErrWorkspaceOperationFailed) {
		t.Fatalf("definitive provider failure: %v", err)
	}
	if _, err = s.CreatePaidTeam(ctx, actor, "", in); !errors.Is(err, billing.ErrWorkspaceOperationFailed) {
		t.Fatalf("terminal failure replay: %v", err)
	}
	key += "-new"
	in.OperationID = key
	customer += "-new"
	failCheckout = true
	if _, err = s.CreatePaidTeam(ctx, actor, "", in); !errors.Is(err, billing.ErrWorkspaceOperationFailed) {
		t.Fatalf("failed order did not release slug: %v", err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.teams WHERE team_id=$1`, in.TeamID).Scan(&count); err != nil || count != 0 {
		t.Fatal("definitively rejected purchase created Team access")
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.users SET status='suspended' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePaidTeam(ctx, actor, "", in); !errors.Is(err, billing.ErrWorkspaceForbidden) {
		t.Fatalf("revoked actor replay: %v", err)
	}
	var cipher []byte
	if err = store.SQL().QueryRowContext(ctx, `SELECT request_ciphertext FROM paperboat.billing_team_orders WHERE operation_id=$1`, key).Scan(&cipher); err != nil || strings.Contains(string(cipher), in.SuccessURL) {
		t.Fatal("request encryption missing")
	}
}
