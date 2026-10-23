package billing

import (
	"encoding/json"
	"net/url"
	"testing"
)

// An overdue send_invoice invoice (pay by invoice, set per customer in Stripe) starts the renewal grace from its due date.
func TestSendInvoiceOverdueStartsGraceAtDueDate(t *testing.T) {
	raw := `{"id":"sub_1","customer":"cus_1","status":"past_due","latest_invoice":{"id":"in_1","billing_reason":"subscription_cycle","status":"open","attempted":false,"amount_remaining":15000,"collection_method":"send_invoice","due_date":1702592000,"status_transitions":{"finalized_at":1700000000}},"items":{"data":[{"id":"si_1","quantity":10,"current_period_start":1700000000,"current_period_end":1731536000,"price":{"id":"price_1"}}]}}`
	var r stripeSubscriptionResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	s, err := r.subscription()
	if err != nil || s.RenewalInvoiceID != "in_1" || s.RenewalFailureAt.Unix() != 1702592000 {
		t.Fatalf("subscription=%+v err=%v", s, err)
	}
}

func TestCheckoutCollectsTaxIDAndTaxOnlyWithTheSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		v := url.Values{}
		HTTPStripeClient{AutomaticTax: on}.businessCheckout(v)
		if v.Get("tax_id_collection[enabled]") != "true" || v.Get("customer_update[address]") != "auto" {
			t.Fatalf("tax id collection missing: %v", v)
		}
		if (v.Get("automatic_tax[enabled]") == "true") != on {
			t.Fatalf("automatic tax with switch %v: %v", on, v)
		}
	}
}
