package billing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/legalconsent"
)

// Every Checkout asks for the terms with the wording the console showed, and every invoice Stripe emails carries it.
func TestStripeCarriesTheConsentWordingAndVersion(t *testing.T) {
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms = append(forms, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cs_1","url":"https://checkout.stripe.test/cs_1","customer":"cus_1","livemode":false}`)
	}))
	defer srv.Close()
	c := HTTPStripeClient{BaseURL: srv.URL, APIKey: "sk_test_secret", Client: srv.Client()}
	if _, err := c.CreateCustomer(context.Background(), "w1", "a@example.test", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSubscriptionCheckout(context.Background(), StripeCheckoutInput{CustomerID: "cus_1", PriceID: "p", Quantity: 1, SuccessURL: "https://x.test/s", CancelURL: "https://x.test/c", IdempotencyKey: "k2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTopupCheckout(context.Background(), StripeTopupInput{CustomerID: "cus_1", PriceID: "p", Quantity: 1, WorkspaceID: "w1", SuccessURL: "https://x.test/s", CancelURL: "https://x.test/c", IdempotencyKey: "k3"}); err != nil {
		t.Fatal(err)
	}
	footer := forms[0].Get("invoice_settings[footer]")
	if !strings.Contains(footer, legalconsent.Version) || !strings.Contains(footer, "payments are not refunded") || !strings.Contains(footer, "Cancel any time") {
		t.Fatalf("customer footer = %q", footer)
	}
	for _, f := range forms[1:] {
		if f.Get("consent_collection[terms_of_service]") != "required" || f.Get("custom_text[terms_of_service_acceptance][message]") != legalconsent.Text {
			t.Fatalf("checkout lacks the terms consent: %v", f)
		}
	}
	if forms[2].Get("invoice_creation[enabled]") != "true" || forms[2].Get("invoice_creation[invoice_data][footer]") != footer {
		t.Fatalf("top-up invoice lacks the footer: %v", forms[2])
	}
}
