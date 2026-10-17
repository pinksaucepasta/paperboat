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

// This opt-in check uses an actual Stripe test account, never a live key.
func TestStripeTestModeCheckoutAndSubscriptionContract(t *testing.T) {
	key, base, price := os.Getenv("PAPERBOAT_TEST_STRIPE_API_KEY"), os.Getenv("PAPERBOAT_TEST_STRIPE_BASE_URL"), os.Getenv("PAPERBOAT_TEST_STRIPE_PRICE_ID")
	if key == "" {
		t.Skip("set Stripe test API key, base URL and recurring price to verify the provider contract")
	}
	if !(strings.HasPrefix(key, "sk_test_") || strings.HasPrefix(key, "rk_test_")) || base == "" || price == "" {
		t.Fatal("an explicit test credential, API base URL and recurring price are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c := HTTPStripeClient{APIKey: key, BaseURL: base, Client: &http.Client{Timeout: 15 * time.Second}}
	operation := "paperboat-test-" + uuid.NewString()
	var customer struct{ ID string }
	if err := c.do(ctx, http.MethodPost, "/v1/customers", operation+"-customer", url.Values{"payment_method": {"pm_card_visa"}, "invoice_settings[default_payment_method]": {"pm_card_visa"}, "metadata[paperboat_test]": {operation}}, &customer); err != nil {
		t.Fatal("create test customer:", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var deleted struct{ Deleted bool }
		if err := c.do(cleanup, http.MethodDelete, "/v1/customers/"+url.PathEscape(customer.ID), "", nil, &deleted); err != nil || !deleted.Deleted {
			t.Error("test customer cleanup failed")
		}
	}()
	checkout, err := c.CreateSubscriptionCheckout(ctx, StripeCheckoutInput{CustomerID: customer.ID, PriceID: price, Quantity: 1, SuccessURL: "https://example.test/success", CancelURL: "https://example.test/cancel", IdempotencyKey: operation + "-checkout"})
	if err != nil {
		t.Fatal("create actual Checkout:", err)
	}
	state, err := c.GetCheckoutSession(ctx, checkout.ProviderSessionID)
	if err != nil || state.CustomerID != customer.ID || state.PriceID != price || state.Quantity != 1 || state.CheckoutOperation != operation+"-checkout" {
		t.Fatal("actual Checkout identity/quantity/operation did not round-trip")
	}
	var expired struct{ Status string }
	if err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions/"+url.PathEscape(state.ID)+"/expire", operation+"-expire", url.Values{}, &expired); err != nil || expired.Status != "expired" {
		t.Fatal("expire unused test Checkout failed")
	}
	var created struct{ ID string }
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions", operation+"-subscription", url.Values{"customer": {customer.ID}, "items[0][price]": {price}, "metadata[paperboat_checkout_operation]": {operation + "-checkout"}, "payment_behavior": {"error_if_incomplete"}}, &created); err != nil {
		t.Fatal("create actual test subscription:", err)
	}
	sub, err := c.GetSubscription(ctx, created.ID)
	if err != nil || sub.CustomerID != customer.ID || sub.PriceID != price || sub.Quantity != 1 || sub.Status != "active" || !sub.PeriodEnd.After(sub.PeriodStart) {
		t.Fatal("actual subscription identity/period decoding failed:", err)
	}
	var before struct {
		LatestInvoice string `json:"latest_invoice"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(sub.ID), "", nil, &before); err != nil {
		t.Fatal("read initial invoice:", err)
	}
	updated, err := c.UpdateSubscriptionSeats(ctx, sub.ID, sub.ItemID, 2, operation+"-seats")
	if err != nil || updated.Quantity != 2 || updated.PeriodStart != sub.PeriodStart || updated.PeriodEnd != sub.PeriodEnd {
		t.Fatal("actual seat update changed its billing period or lost quantity:", err)
	}
	var after struct {
		LatestInvoice struct {
			ID, Status string
			AmountPaid int64 `json:"amount_paid"`
		} `json:"latest_invoice"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(sub.ID)+"?expand%5B%5D=latest_invoice", "", nil, &after); err != nil {
		t.Fatal("read seat-change invoice:", err)
	}
	if after.LatestInvoice.ID == before.LatestInvoice || after.LatestInvoice.Status != "paid" || after.LatestInvoice.AmountPaid <= 0 {
		t.Fatal("seat increase did not immediately invoice and collect its proration")
	}
}

// Portal probes create no subscriptions and can run with lifecycle webhooks
// enabled. Their exact test customers are removed on every exit path.
func TestStripeTestModeScopedPortalContract(t *testing.T) {
	key, base := os.Getenv("PAPERBOAT_TEST_STRIPE_API_KEY"), os.Getenv("PAPERBOAT_TEST_STRIPE_BASE_URL")
	if key == "" {
		t.Skip("explicit Stripe test credentials and scoped portal configurations required")
	}
	if !(strings.HasPrefix(key, "sk_test_") || strings.HasPrefix(key, "rk_test_")) || base == "" {
		t.Fatal("explicit test credential and API base required")
	}
	for _, scope := range []string{"PERSONAL", "TEAM"} {
		t.Run(scope, func(t *testing.T) {
			configuration := os.Getenv("PAPERBOAT_TEST_STRIPE_" + scope + "_PORTAL_CONFIGURATION_ID")
			if configuration == "" {
				t.Fatal("scoped portal configuration required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			c := HTTPStripeClient{APIKey: key, BaseURL: base, Client: &http.Client{Timeout: 15 * time.Second}}
			operation := "portal-test-" + uuid.NewString()
			customer, err := c.CreateCustomer(ctx, operation, "portal@example.test", operation+"-customer")
			if err != nil {
				t.Fatal("create test portal customer:", err)
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var deleted struct{ Deleted bool }
				if err := c.do(cleanup, http.MethodDelete, "/v1/customers/"+url.PathEscape(customer), "", nil, &deleted); err != nil || !deleted.Deleted {
					t.Error("test portal customer cleanup failed")
				}
			}()
			session, err := c.CreatePortal(ctx, StripePortalInput{CustomerID: customer, ReturnURL: "https://example.test/billing", IdempotencyKey: operation, ConfigurationID: configuration})
			if err != nil || session.URL == "" {
				t.Fatal("actual scoped portal contract:", err)
			}
		})
	}
}
