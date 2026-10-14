package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPStripeClientGetPriceValidatesRecurringLicensedPrice(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"valid", `{"id":"price_ok","object":"price","active":true,"livemode":false,"currency":"usd","product":"prod_ok","type":"recurring","unit_amount":1200,"recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}}`, false},
		{"inactive-archive", `{"id":"price_ok","object":"price","active":false,"livemode":false,"currency":"usd","product":"prod_ok","type":"recurring","unit_amount":1200,"recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}}`, false},
		{"metered", `{"id":"price_ok","object":"price","active":true,"livemode":false,"currency":"usd","product":"prod_ok","type":"recurring","unit_amount":1200,"recurring":{"interval":"month","interval_count":1,"usage_type":"metered"}}`, true},
		{"custom-amount", `{"id":"price_ok","object":"price","active":true,"livemode":false,"currency":"usd","product":"prod_ok","type":"recurring","unit_amount":null,"recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}}`, true},
		{"invalid-currency", `{"id":"price_ok","object":"price","active":true,"livemode":false,"currency":"US","product":"prod_ok","type":"recurring","unit_amount":1200,"recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}}`, true},
		{"compound-interval", `{"id":"price_ok","object":"price","active":true,"livemode":false,"currency":"usd","product":"prod_ok","type":"recurring","unit_amount":1200,"recurring":{"interval":"month","interval_count":2,"usage_type":"licensed"}}`, true},
		{"unsupported-daily", `{"id":"price_ok","object":"price","active":true,"livemode":false,"currency":"usd","product":"prod_ok","type":"recurring","unit_amount":1200,"recurring":{"interval":"day","interval_count":1,"usage_type":"licensed"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/prices/price_ok" {
					t.Fatalf("request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer sk_test_operator" {
					t.Fatal("missing Stripe authorization")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			price, err := (HTTPStripeClient{BaseURL: server.URL, APIKey: "sk_test_operator"}).GetPrice(context.Background(), "price_ok")
			if tc.wantErr != (err != nil) {
				t.Fatalf("price=%+v err=%v", price, err)
			}
			if !tc.wantErr && (price.ProductID != "prod_ok" || price.UnitAmount != 1200 || price.Currency != "usd" || price.Interval != "month" || (tc.name == "inactive-archive" && price.Active)) {
				t.Fatalf("price=%+v", price)
			}
		})
	}
}

func TestOperatorInputValidation(t *testing.T) {
	negative := int64(-1)
	if validateProductInput(ProductInput{DisplayName: "Plan", ProviderPriceID: "price", CatalogType: "personal_subscription", AllowanceBytes: &negative}) == nil {
		t.Fatal("negative allowance accepted")
	}
	if !invalidPolicy(OperatorPolicy{PersonalAllowanceBytes: &negative, Revision: 1}) {
		t.Fatal("negative policy accepted")
	}
}
