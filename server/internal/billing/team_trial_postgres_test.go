package billing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// An account's first Team starts a 14-day trial: Checkout collects the card, the trialing
// subscription is a paid team, the reminder mails owners, and a second team has no trial.
func TestTeamTrialFirstTeamOnlyWithCardAndReminder(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	actor, team, key := "usr_trial_"+suffix, "trial-team-"+suffix, "trial-op-"+suffix
	insertUser(t, store, actor, actor+"@example.test")
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.users SET email_verified=true WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	customer, session, subscription, price := "cus_t"+suffix, "cs_t"+suffix, "sub_t"+suffix, "price_t"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,'team-month','stripe',$2,$3,'team_subscription','{}',true,1500,'usd','month')`, "bp_t"+suffix, "prod_t"+suffix, price); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE 'team-trial-ending:` + subscription + `:%'`,
			`DELETE FROM paperboat.stripe_events WHERE subscription_id='` + subscription + `'`,
			`DELETE FROM paperboat.billing_workspaces WHERE provider_subscription_id='` + subscription + `'`,
			`DELETE FROM paperboat.billing_team_orders WHERE actor_user_id='` + actor + `'`,
			`DELETE FROM paperboat.team_members WHERE team_id LIKE '` + team + `%'`,
			`DELETE FROM paperboat.teams WHERE team_id LIKE '` + team + `%'`,
			`DELETE FROM paperboat.audit_events WHERE actor_user_id='` + actor + `'`,
			`DELETE FROM paperboat.users WHERE id='` + actor + `'`,
			`DELETE FROM paperboat.billing_products WHERE id='bp_t` + suffix + `'`,
		} {
			store.SQL().ExecContext(c, q)
		}
	})
	var trialForms []string
	// Stripe reports a card-collecting trial Checkout as paid.
	checkoutPayment := "paid"
	customers := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/customers":
			customers++
			json.NewEncoder(w).Encode(map[string]any{"id": customer + strings.Repeat("x", customers-1)})
		case r.Method == "POST" && r.URL.Path == "/v1/checkout/sessions":
			r.ParseForm()
			trialForms = append(trialForms, r.Form.Get("subscription_data[trial_period_days]")+"|"+r.Form.Get("payment_method_collection")+"|"+r.Form.Get("subscription_data[trial_settings][end_behavior][missing_payment_method]"))
			json.NewEncoder(w).Encode(map[string]any{"id": session + strings.Repeat("x", len(trialForms)-1), "customer": r.Form.Get("customer"), "url": "https://checkout.example.test/s"})
		case r.Method == "GET" && r.URL.Path == "/v1/checkout/sessions/"+session:
			json.NewEncoder(w).Encode(map[string]any{"id": session, "customer": customer, "status": "complete", "mode": "subscription", "payment_status": checkoutPayment, "subscription": subscription, "metadata": map[string]string{"paperboat_checkout_operation": "team-create:" + key}, "line_items": map[string]any{"data": []any{map[string]any{"quantity": 2, "price": map[string]string{"id": price}}}}})
		case r.Method == "GET" && r.URL.Path == "/v1/subscriptions/"+subscription:
			json.NewEncoder(w).Encode(map[string]any{"id": subscription, "customer": customer, "status": "trialing", "metadata": map[string]string{"paperboat_checkout_operation": "team-create:" + key}, "items": map[string]any{"data": []any{map[string]any{"id": "si_t" + suffix, "quantity": 2, "current_period_start": time.Now().Unix() - 60, "current_period_end": time.Now().Unix() + 14*86400, "price": map[string]string{"id": price}}}}})
		default:
			t.Errorf("unexpected provider %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	s := billing.NewWorkspaceService(store, billing.HTTPStripeClient{BaseURL: provider.URL, APIKey: "sk_test_fixture", Client: provider.Client()}, "trial-encryption-key")
	s.ConfigureTeamTrial(14)
	s.ConfigureNotificationMail(config.TransactionalMail{From: "Paperboat <noreply@example.test>", ConsoleOrigin: "https://console.example.test"}, true)
	trialDays := func() int {
		t.Helper()
		o, err := s.Overview(ctx, actor, "")
		if err != nil || o.TeamTrialDays == nil {
			t.Fatalf("personal overview trial: %+v %v", o.TeamTrialDays, err)
		}
		return *o.TeamTrialDays
	}
	if got := trialDays(); got != 14 {
		t.Fatalf("new account trial days=%d", got)
	}
	in := billing.TeamCreationInput{OperationID: key, TeamID: team, Seats: 2, Interval: "month", SuccessURL: "https://console.example.test/ok", CancelURL: "https://console.example.test/no"}
	if _, err := s.CreatePaidTeam(ctx, actor, actor+"@example.test", in); err != nil {
		t.Fatal(err)
	}
	event := billing.StripeWebhook{ID: "evt_t" + suffix, Type: "checkout.session.completed"}
	event.Data.Object.Object, event.Data.Object.ID = "checkout.session", session
	if processed, err := s.HandleWebhook(ctx, event); err != nil || !processed {
		t.Fatalf("trial fulfillment: %v %v", processed, err)
	}
	var state string
	var entitled bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT state,paperboat.team_paid_entitled($1) FROM paperboat.billing_workspaces WHERE team_id=$1`, team).Scan(&state, &entitled); err != nil || state != "trialing" || !entitled {
		t.Fatalf("trialing team state=%s entitled=%v err=%v", state, entitled, err)
	}
	if got := trialDays(); got != 0 {
		t.Fatalf("trial offered twice: %d", got)
	}
	reminder := billing.StripeWebhook{ID: "evt_tw" + suffix, Type: billing.TrialWillEndEvent}
	reminder.Data.Object.Object, reminder.Data.Object.ID = "subscription", subscription
	for range 2 {
		if _, err := s.HandleWebhook(ctx, reminder); err != nil {
			t.Fatalf("trial reminder: %v", err)
		}
	}
	var mails int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.transactional_mail_outbox WHERE mail_id=$1`, "team-trial-ending:"+subscription+":"+actor).Scan(&mails); err != nil || mails != 1 {
		t.Fatalf("trial reminder mails=%d err=%v", mails, err)
	}
	second := in
	second.OperationID, second.TeamID = key+"-2", team+"-2"
	if _, err := s.CreatePaidTeam(ctx, actor, actor+"@example.test", second); err != nil {
		t.Fatal(err)
	}
	if len(trialForms) != 2 || trialForms[0] != "14|always|cancel" || trialForms[1] != "||" {
		t.Fatalf("checkout trial params=%q", trialForms)
	}
}
