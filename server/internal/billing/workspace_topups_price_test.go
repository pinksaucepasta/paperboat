package billing_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

func TestTopupOneTimePriceProviderContract(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		amount      string
		wantErr     bool
	}{
		{"one-time", `"type":"one_time","recurring":null`, "500", false},
		{"missing-amount", `"type":"one_time","recurring":null`, "null", true},
		{"zero-amount", `"type":"one_time","recurring":null`, "0", true},
		{"recurring-masquerade", `"type":"one_time","recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}`, "500", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":"price_topup","object":"price","active":true,"currency":"usd","product":"prod_topup","unit_amount":%s,%s}`, tc.amount, tc.extra)
			}))
			defer server.Close()
			price, err := (billing.HTTPStripeClient{BaseURL: server.URL, APIKey: "sk_test_fixture"}).GetPrice(context.Background(), "price_topup")
			if (err != nil) != tc.wantErr {
				t.Fatalf("one-time price verification error=%v", err)
			}
			if !tc.wantErr && (price.UnitAmount != 500 || price.Currency != "usd" || price.Interval != "one_time" || price.Recurring) {
				t.Fatalf("incorrect one-time price: %+v", price)
			}
		})
	}
}
