package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

type topupStripeFake struct {
	workspaceStripeFake
	states  map[string]billing.StripeTopupState
	calls   int
	timeout bool
}

func (f *topupStripeFake) CreateTopupCheckout(_ context.Context, in billing.StripeTopupInput) (billing.CheckoutSession, error) {
	f.calls++
	id := "cs_" + in.IdempotencyKey
	f.states[id] = billing.StripeTopupState{ID: id, CustomerID: in.CustomerID, WorkspaceID: in.WorkspaceID, Operation: in.IdempotencyKey, PriceID: in.PriceID, Quantity: in.Quantity, AmountTotal: 500 * int64(in.Quantity), Currency: "usd", Mode: "payment", Status: "open", PaymentStatus: "unpaid", CreatedAt: time.Now().UTC().Truncate(time.Second), URL: "https://checkout.example.test/topup"}
	if f.timeout {
		f.timeout = false
		return billing.CheckoutSession{}, context.DeadlineExceeded
	}
	return billing.CheckoutSession{ProviderSessionID: id, ProviderCustomerID: in.CustomerID, URL: "https://checkout.example.test/topup"}, nil
}
func (f *topupStripeFake) GetTopupCheckout(_ context.Context, id string) (billing.StripeTopupState, error) {
	return f.states[id], nil
}

func TestWorkspaceTopupCheckoutFulfillmentAndLedgerPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	user, free, team, code, admin := "usr_topup_"+x, "usr_topup_free_"+x, "team-topup-"+x, "topup_"+x, "op_topup_"+x
	for _, u := range []string{user, free} {
		insertUser(t, store, u, u+"@example.test")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,role,status) VALUES($1,$1||'@example.test','unused','admin','active')`, admin)
	amount := int64(100_000_000_000)
	input := billing.ProductInput{DisplayName: "100 GB top-up", ProviderPriceID: "price_" + x, CatalogType: "top_up", Active: true, BytesAmount: &amount}
	t.Cleanup(func() {
		clean := func(q string, args ...any) {
			if _, err := store.SQL().ExecContext(context.Background(), q, args...); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
		clean(`DELETE FROM paperboat.billing_workspace_topup_events WHERE provider_session_id IN (SELECT provider_session_id FROM paperboat.billing_workspace_topups WHERE workspace_id IN ($1,$2))`, "personal:"+user, "team:"+team)
		clean(`DELETE FROM paperboat.billing_workspace_topup_events WHERE event_id LIKE $1`, "%"+x)
		clean(`DELETE FROM paperboat.billing_workspace_topups WHERE workspace_id IN ($1,$2)`, "personal:"+user, "team:"+team)
		clean(`DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id IN ($1,$2,$3)`, "personal:"+user, "team:"+team, "personal:"+free)
		clean(`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id IN ($1,$2,$3)`, "personal:"+user, "team:"+team, "personal:"+free)
		clean(`DELETE FROM paperboat.billing_workspaces WHERE id IN ($1,$2,$3)`, "personal:"+user, "team:"+team, "personal:"+free)
		clean(`DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		clean(`DELETE FROM paperboat.billing_operator_operations WHERE actor_user_id=$1`, admin)
		// Immutable audit receipts intentionally outlive the disposable fixture.
		clean(`DELETE FROM paperboat.team_operations WHERE account_id=$1`, user)
		clean(`DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		clean(`DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		clean(`DELETE FROM paperboat.operator_accounts WHERE id=$1`, admin)
		clean(`DELETE FROM paperboat.users WHERE id IN ($1,$2)`, user, free)
	})
	price := billing.StripePrice{ID: input.ProviderPriceID, ProductID: "prod_" + x, Currency: "usd", Interval: "one_time", UnitAmount: 500, Active: true}
	operator := billing.NewOperatorService(store, operatorPriceReader{price})
	product, err := operator.PutProduct(ctx, admin, "catalog_"+x, code, "configure top-up", input)
	if err != nil || product.BytesAmount == nil || *product.BytesAmount != amount {
		t.Fatalf("top-up catalog: %+v %v", product, err)
	}
	badPrice := price
	badPrice.Recurring = true
	badPrice.Licensed = true
	badPrice.Interval = "month"
	if _, err = billing.NewOperatorService(store, operatorPriceReader{badPrice}).PutProduct(ctx, admin, "wrong_"+x, "wrong_"+x, "reject recurring top-up", input); !errors.Is(err, billing.ErrOperatorBillingInvalid) {
		t.Fatalf("recurring top-up accepted: %v", err)
	}
	_, err = teams.NewService(store).Create(ctx, user, teams.CreateRequest{OperationID: "team_" + x, TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []int{0, -1, 101} {
		if _, e := billing.NewWorkspaceService(nil, nil, "").Topup(ctx, user, "", code, "invalid", "https://dashboard.example.test/billing", "https://dashboard.example.test/billing", 1, q); !errors.Is(e, billing.ErrTopupQuantity) {
			t.Fatalf("quantity %d accepted: %v", q, e)
		}
	}
	f := &topupStripeFake{states: map[string]billing.StripeTopupState{}}
	s := billing.NewWorkspaceService(store, f, "topup-test-key")
	_, err = s.Overview(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Overview(ctx, free, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Overview(ctx, user, team)
	if err != nil {
		t.Fatal(err)
	}
	buy := func(actor, team, key string) (billing.CheckoutSession, error) {
		return s.Topup(ctx, actor, team, code, key, "https://dashboard.example.test/billing?done=1", "https://dashboard.example.test/billing", 1, 3)
	}
	if _, err = buy(free, "", "free_"+x); !errors.Is(err, billing.ErrTopupRequiresPaid) {
		t.Fatalf("Free purchase: %v", err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	end := start.AddDate(0, 1, 0)
	for _, w := range []string{"personal:" + user, "team:" + team} {
		exec(`UPDATE paperboat.billing_workspaces SET product_code=$2,provider_customer_id=$3,state='active',quantity=1,period_start=$4,period_end=$5,bandwidth_anchor=$4 WHERE id=$1`, w, code, "cus_"+w, start, end)
	}
	page, err := s.Products(ctx, user, "", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range page.Items {
		if p.Code == code {
			found = p.CatalogType == "top_up" && p.UnitAmount != nil && *p.UnitAmount == 500 && p.UnitBytes != nil && *p.UnitBytes == amount && p.MaxQuantity != nil && *p.MaxQuantity == 100
		}
	}
	if !found {
		t.Fatal("verified top-up absent from workspace product catalog")
	}
	remaining := func(w string, at time.Time) int64 {
		t.Helper()
		var n int64
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var e error
			n, e = billing.WorkspaceTopupRemainingTx(ctx, tx, w, at)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	event := func(id, session, kind string, at time.Time) billing.StripeWebhook {
		e := billing.StripeWebhook{ID: id, Type: kind, Created: at.Unix()}
		e.Data.Object.ID = session
		e.Data.Object.Object = "checkout.session"
		return e
	}
	for _, scope := range []string{"", team} {
		key := "purchase_" + scope + x
		f.timeout = true
		if _, err = buy(user, scope, key); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
			t.Fatalf("unknown outcome=%v", err)
		}
		receipt, err := buy(user, scope, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, e := s.Topup(ctx, user, scope, code, key, "https://dashboard.example.test/billing?done=1", "https://dashboard.example.test/billing", 1, 2); !errors.Is(e, billing.ErrIdempotencyConflict) {
			t.Fatalf("quantity replay conflict: %v", e)
		}
		calls := f.calls
		if _, err = buy(user, scope, key); err != nil || f.calls != calls {
			t.Fatalf("replay calls=%d err=%v", f.calls-calls, err)
		}
		if _, err = s.Topup(ctx, user, scope, code, key, "https://dashboard.example.test/different", "https://dashboard.example.test/billing", 1, 3); !errors.Is(err, billing.ErrIdempotencyConflict) {
			t.Fatalf("changed replay: %v", err)
		}
		state := f.states[receipt.ProviderSessionID]
		if state.Quantity != 3 || state.AmountTotal != 1500 {
			t.Fatal("provider request did not retain the requested quantity and verified unit price")
		}
		w := state.WorkspaceID
		if remaining(w, time.Now().UTC()) != 0 {
			t.Fatal("Checkout redirect created credit")
		}
		unpaid := event("unpaid_"+key, state.ID, "checkout.session.completed", time.Now().UTC())
		if _, err = s.HandleWebhook(ctx, unpaid); err != nil {
			t.Fatal(err)
		}
		if remaining(w, time.Now().UTC()) != 0 {
			t.Fatal("unpaid event created credit")
		}
		state.Status = "complete"
		state.PaymentStatus = "paid"
		f.states[state.ID] = state
		paidAt := time.Now().UTC().Truncate(time.Second)
		paid := event("paid_"+key, state.ID, "checkout.session.async_payment_succeeded", paidAt)
		if scope == "" {
			paid.Type = "checkout.session.completed"
			paid.Data.Object.PaymentStatus = "paid"
		}
		// A provider snapshot alone cannot override operation/customer/price binding.
		for _, mutate := range []func(*billing.StripeTopupState){
			func(v *billing.StripeTopupState) { v.CustomerID = "cus_foreign" }, func(v *billing.StripeTopupState) { v.PriceID = "price_foreign" }, func(v *billing.StripeTopupState) { v.Quantity = 2 }, func(v *billing.StripeTopupState) { v.AmountTotal = 500 }, func(v *billing.StripeTopupState) { v.Currency = "eur" }, func(v *billing.StripeTopupState) { v.WorkspaceID = "personal:" + free }, func(v *billing.StripeTopupState) { v.Operation = "foreign" },
		} {
			bad := state
			mutate(&bad)
			f.states[state.ID] = bad
			if _, err = s.HandleWebhook(ctx, paid); err != nil {
				t.Fatal(err)
			}
			if remaining(w, paidAt) != 0 {
				t.Fatal("mismatched event credited")
			}
		}
		unconfirmed := state
		unconfirmed.PaymentStatus = "unpaid"
		f.states[state.ID] = unconfirmed
		if _, err = s.HandleWebhook(ctx, paid); !errors.Is(err, billing.ErrProviderOutcomeUnknown) {
			t.Fatalf("paid event with unconfirmed provider snapshot: %v", err)
		}
		if remaining(w, paidAt) != 0 {
			t.Fatal("unconfirmed paid event credited")
		}
		f.states[state.ID] = state
		// Revoking paid benefits before delivery cannot grant a Free workspace credit.
		exec(`UPDATE paperboat.billing_workspaces SET state='canceled' WHERE id=$1`, w)
		if _, err = s.HandleWebhook(ctx, paid); err != nil {
			t.Fatal(err)
		}
		if remaining(w, paidAt) != 0 {
			t.Fatal("Free workspace credited")
		}
		exec(`UPDATE paperboat.billing_workspaces SET state='active',period_end=$2 WHERE id=$1`, w, end)
		type outcome struct {
			processed bool
			err       error
		}
		results := make(chan outcome, 2)
		for range 2 {
			go func() { p, e := s.HandleWebhook(ctx, paid); results <- outcome{p, e} }()
		}
		processedCount := 0
		for range 2 {
			r := <-results
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.processed {
				processedCount++
			}
		}
		if processedCount != 1 {
			t.Fatalf("concurrent paid event processed %d times", processedCount)
		}
		if remaining(w, paidAt) != amount*3 {
			t.Fatal("paid bytes not credited")
		}
		if processed, err := s.HandleWebhook(ctx, paid); err != nil || processed {
			t.Fatalf("duplicate processed=%v err=%v", processed, err)
		}
		second := paid
		second.ID = "duplicate_session_" + key
		if _, err = s.HandleWebhook(ctx, second); err != nil {
			t.Fatal(err)
		}
		if remaining(w, paidAt) != amount*3 {
			t.Fatal("duplicate session credited twice")
		}
		if remaining(w, paidAt.Add(-time.Nanosecond)) != 0 {
			t.Fatal("future purchase credited")
		}
		future := paidAt.AddDate(2, 3, 0)
		if remaining(w, future) != amount*3 {
			t.Fatal("permanent credits expired across months/years")
		}

		secondReceipt, e := buy(user, scope, "z_second_"+key)
		if e != nil {
			t.Fatal(e)
		}
		secondState := f.states[secondReceipt.ProviderSessionID]
		secondState.Status = "complete"
		secondState.PaymentStatus = "paid"
		f.states[secondState.ID] = secondState
		secondPaid := event("second_paid_"+key, secondState.ID, "checkout.session.async_payment_succeeded", time.Now().UTC())
		if _, e = s.HandleWebhook(ctx, secondPaid); e != nil {
			t.Fatal(e)
		}
		consumeAt := time.Now().UTC().AddDate(3, 0, 0)

		var used int64
		err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var e error
			used, e = billing.ConsumeWorkspaceTopupsTx(ctx, tx, w, amount*3+5, consumeAt)
			return e
		})
		if err != nil || used != amount*3+5 || remaining(w, consumeAt) != amount*3-5 {
			t.Fatalf("bounded consumption=%d %v", used, err)
		}
		var oldRemaining int64
		if e = store.SQL().QueryRowContext(ctx, `SELECT remaining_bytes FROM paperboat.billing_workspace_topups WHERE provider_session_id=$1`, receipt.ProviderSessionID).Scan(&oldRemaining); e != nil || oldRemaining != 0 {
			t.Fatalf("oldest purchase remaining=%d err=%v", oldRemaining, e)
		}
		if e = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, err := billing.ConsumeWorkspaceTopupsTx(ctx, tx, w, amount*10, consumeAt)
			return err
		}); e != nil || remaining(w, consumeAt) != 0 {
			t.Fatalf("bounded final consumption: %v", e)
		}
	}
	if remaining("personal:"+free, time.Now().UTC()) != 0 {
		t.Fatal("foreign workspace received credit")
	}
}
