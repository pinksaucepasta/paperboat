package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

func TestStripePurchasedSeatsImmediateInvoiceAndScheduledReduction(t *testing.T) {
	start, end := time.Unix(1780000000, 0).UTC(), time.Unix(1782592000, 0).UTC()
	in := billing.SeatChange{SubscriptionID: "sub_team", CustomerID: "cus_team", ItemID: "si_team", PriceID: "price_team", PreviousQuantity: 2, Quantity: 3, PeriodStart: start, PeriodEnd: end, ProrationAt: 1781000000}
	updates, previews, schedules := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/invoices/create_preview":
			previews++
			amount := int64(4500)
			if r.Form.Get("subscription") != "sub_team" || r.Form.Get("subscription_details[items][0][quantity]") != "3" {
				t.Error("preview subscription/quantity mismatch")
			}
			if r.Form.Get("subscription_details[proration_behavior]") == "always_invoice" {
				amount = 750
				if r.Form.Get("subscription_details[proration_behavior]") != "always_invoice" || r.Form.Get("subscription_details[proration_date]") != "1781000000" {
					t.Error("preview proration mismatch")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"amount_due": amount, "currency": "usd"})
		case "GET /v1/subscriptions/sub_team":
			json.NewEncoder(w).Encode(map[string]any{"id": "sub_team", "customer": "cus_team", "schedule": nil, "items": map[string]any{"data": []any{map[string]any{"id": "si_team", "quantity": 3, "price": map[string]string{"id": "price_team"}, "current_period_start": start.Unix(), "current_period_end": end.Unix()}}}})
		case "POST /v1/subscriptions/sub_team":
			updates++
			if r.Header.Get("Idempotency-Key") != "buy-operation" || r.Form.Get("proration_behavior") != "always_invoice" || r.Form.Get("payment_behavior") != "error_if_incomplete" || r.Form.Get("proration_date") != "1781000000" || r.Form.Has("billing_cycle_anchor") {
				t.Error("purchase must charge now without resetting billing date")
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "sub_team", "customer": "cus_team", "status": "active", "latest_invoice": map[string]any{"id": "in_paid", "amount_due": 750, "currency": "usd", "status": "paid"}, "items": map[string]any{"data": []any{map[string]any{"id": "si_team", "quantity": 3, "current_period_start": start.Unix(), "current_period_end": end.Unix(), "price": map[string]string{"id": "price_team"}}}}})
		case "GET /v1/prices/price_team":
			json.NewEncoder(w).Encode(map[string]any{"id": "price_team", "object": "price", "currency": "usd", "product": "prod_team", "type": "recurring", "active": true, "unit_amount": 1500, "recurring": map[string]any{"interval": "month", "interval_count": 1, "usage_type": "licensed"}})
		case "POST /v1/subscription_schedules":
			if r.Form.Get("from_subscription") != "sub_team" {
				t.Error("schedule must attach to existing subscription")
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "sched_team"})
		case "POST /v1/subscription_schedules/sched_team":
			schedules++
			for k, want := range map[string]string{"proration_behavior": "none", "phases[0][items][0][quantity]": "3", "phases[1][items][0][quantity]": "2", "phases[0][end_date]": "1782592000", "phases[1][start_date]": "1782592000", "phases[1][proration_behavior]": "none", "end_behavior": "release"} {
				if r.Form.Get(k) != want {
					t.Errorf("schedule %s=%q want %q", k, r.Form.Get(k), want)
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "sched_team"})
		default:
			t.Errorf("unexpected Stripe request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	client := billing.HTTPStripeClient{BaseURL: server.URL, APIKey: "sk_test_seats", Client: server.Client()}
	quote, err := client.PreviewSeats(context.Background(), in)
	if err != nil || quote.AmountDueNow != 750 || quote.NextInvoiceAmount != 4500 || !quote.NextInvoiceAt.Equal(end) {
		t.Fatalf("preview=%+v error=%v", quote, err)
	}
	in.Preview = quote
	paid, err := client.ChangeSeats(context.Background(), in, "buy-operation")
	if err != nil || paid != quote {
		t.Fatalf("purchase=%+v error=%v", paid, err)
	}
	in.PreviousQuantity, in.Quantity = 3, 2
	in.Preview = billing.SeatPreview{Currency: "usd", NextInvoiceAmount: 3000, NextInvoiceAt: end}
	removal, err := client.ChangeSeats(context.Background(), in, "remove-operation")
	if err != nil || removal.AmountDueNow != 0 {
		t.Fatalf("removal=%+v error=%v", removal, err)
	}
	if updates != 1 || previews != 2 || schedules != 1 {
		t.Fatalf("unexpected charges/previews/schedules: %d/%d/%d", updates, previews, schedules)
	}
}
