package billing

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This check never pays or creates a subscription. The real provider must accept
// payment-mode Checkout, return its exact binding, and expire the unused session.
func TestStripeTestModeTopupCheckoutContract(t *testing.T) {
	key, base, price := os.Getenv("PAPERBOAT_TEST_STRIPE_API_KEY"), os.Getenv("PAPERBOAT_TEST_STRIPE_BASE_URL"), os.Getenv("PAPERBOAT_TEST_STRIPE_TOPUP_PRICE_ID")
	if key == "" || price == "" {
		t.Skip("set an explicit Stripe test credential, API base and one-time price")
	}
	if !(strings.HasPrefix(key, "sk_test_") || strings.HasPrefix(key, "rk_test_")) || base == "" {
		t.Fatal("only an explicit test credential and API base are permitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := HTTPStripeClient{BaseURL: base, APIKey: key, Client: &http.Client{Timeout: 15 * time.Second}}
	verified, err := c.GetPrice(ctx, price)
	if err != nil || verified.Recurring || verified.Interval != "one_time" || verified.UnitAmount != 500 || verified.Currency != "usd" {
		t.Fatal("configured top-up is not the verified $5 one-time USD price", err)
	}
	operation := "topup-probe-" + uuid.NewString()
	workspace := "personal:" + operation
	customer, err := c.CreateCustomer(ctx, workspace, operation+"@example.test", operation+":customer")
	if err != nil {
		t.Fatal("create test customer", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var result struct{ Deleted bool }
		if e := c.do(cleanup, http.MethodDelete, "/v1/customers/"+url.PathEscape(customer), "", nil, &result); e != nil || !result.Deleted {
			t.Error("test customer cleanup failed")
		}
	}()
	in := StripeTopupInput{Quantity: 3, CustomerID: customer, PriceID: price, WorkspaceID: workspace, IdempotencyKey: operation, SuccessURL: "https://dashboard.example.test/billing?done=1", CancelURL: "https://dashboard.example.test/billing"}
	result, err := c.CreateTopupCheckout(ctx, in)
	if err != nil {
		t.Fatal("create unused payment Checkout", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var expired struct{ Status string }
		if e := c.do(cleanup, http.MethodPost, "/v1/checkout/sessions/"+url.PathEscape(result.ProviderSessionID)+"/expire", operation+":expire", url.Values{}, &expired); e != nil || expired.Status != "expired" {
			t.Error("unused Checkout expiration failed")
		}
	}()
	replay, err := c.CreateTopupCheckout(ctx, in)
	if err != nil || replay.ProviderSessionID != result.ProviderSessionID {
		t.Fatal("provider idempotency did not retain the same payment session", err)
	}
	state, err := c.GetTopupCheckout(ctx, result.ProviderSessionID)
	if err != nil || state.ID != result.ProviderSessionID || !topupIdentity(state, topupRequest{StripeTopupInput: in}) || state.AmountTotal != 1500 || state.Currency != "usd" || state.Status != "open" || state.PaymentStatus != "unpaid" {
		t.Fatal("unused payment Checkout binding did not round-trip", err)
	}
}
