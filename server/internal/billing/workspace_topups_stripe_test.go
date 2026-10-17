package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

func TestTopupCheckoutProviderPaymentAndIdentity(t *testing.T) {
	truncated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_fixture" || r.Header.Get("Stripe-Version") != billing.StripeAPIVersion {
			t.Error("provider authentication/version missing")
		}
		switch r.Method {
		case "POST":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"mode": "payment", "customer": "cus_fixture", "line_items[0][price]": "price_fixture", "line_items[0][quantity]": "1", "metadata[workspace_id]": "team:fixture", "metadata[paperboat_topup_operation]": "op_fixture", "success_url": "https://dashboard.example.test/billing?paid=1", "cancel_url": "https://dashboard.example.test/billing"} {
				if r.Form.Get(key) != want {
					t.Errorf("payment Checkout %s mismatch", key)
				}
			}
			if r.Header.Get("Idempotency-Key") != "op_fixture" || r.Form.Has("payment_method_types") {
				t.Error("unsafe Checkout operation/payment method contract")
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "cs_fixture", "customer": "cus_fixture", "url": "https://checkout.example.test/session"})
		case "GET":
			if r.URL.Path != "/v1/checkout/sessions/cs_fixture" || r.URL.Query().Get("expand[]") != "line_items" {
				t.Error("session identity/line item expansion missing")
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "cs_fixture", "customer": "cus_fixture", "mode": "payment", "status": "complete", "payment_status": "paid", "created": 1700000000, "metadata": map[string]string{"workspace_id": "team:fixture", "paperboat_topup_operation": "op_fixture"}, "line_items": map[string]any{"has_more": truncated, "data": []any{map[string]any{"quantity": 1, "price": map[string]string{"id": "price_fixture"}}}}})
		default:
			t.Error("unexpected provider method")
		}
	}))
	defer server.Close()
	c := billing.HTTPStripeClient{BaseURL: server.URL, APIKey: "sk_test_fixture"}
	result, err := c.CreateTopupCheckout(context.Background(), billing.StripeTopupInput{CustomerID: "cus_fixture", PriceID: "price_fixture", WorkspaceID: "team:fixture", IdempotencyKey: "op_fixture", SuccessURL: "https://dashboard.example.test/billing?paid=1", CancelURL: "https://dashboard.example.test/billing"})
	if err != nil || result.ProviderSessionID != "cs_fixture" {
		t.Fatalf("payment Checkout: %v", err)
	}
	state, err := c.GetTopupCheckout(context.Background(), result.ProviderSessionID)
	if err != nil || state.PaymentStatus != "paid" || state.Mode != "payment" || state.WorkspaceID != "team:fixture" || state.Operation != "op_fixture" || state.PriceID != "price_fixture" || state.Quantity != 1 {
		t.Fatalf("paid session binding: %+v %v", state, err)
	}
	truncated = true
	state, err = c.GetTopupCheckout(context.Background(), result.ProviderSessionID)
	if err != nil || state.PriceID != "" || state.Quantity != 0 {
		t.Fatal("partial line-item collection accepted")
	}
}
