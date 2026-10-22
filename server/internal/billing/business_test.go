package billing

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeStripeBusiness answers the business billing calls and records each request.
func fakeStripeBusiness(t *testing.T) (HTTPStripeClient, *[]string) {
	t.Helper()
	var mu sync.Mutex
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+form.Encode())
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/invoices":
			io.WriteString(w, `{"data":[{"id":"in_2","number":"PB-2","status":"open","currency":"eur","total":15000,"amount_due":15000,"total_taxes":[{"amount":2500}],"created":1700000000,"period_start":1700000000,"period_end":1731536000,"due_date":1702592000,"hosted_invoice_url":"https://pay.example.test/in_2","invoice_pdf":"https://files.example.test/in_2.pdf"},{"id":"in_d","status":"draft"}],"has_more":true}`)
		case r.Method == "GET" && r.URL.Path == "/v1/customers/cus_1":
			io.WriteString(w, `{"id":"cus_1","email":"billing@example.test","name":"Acme","address":{"line1":"1 Road","city":"Berlin","postal_code":"10115","country":"DE"},"tax_ids":{"data":[{"id":"txi_old","type":"eu_vat","value":"DE000000000","verification":{"status":"verified"}}]}}`)
		case r.Method == "POST" && r.URL.Path == "/v1/customers/cus_1":
			io.WriteString(w, `{"id":"cus_1"}`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/customers/cus_1/tax_ids/"):
			io.WriteString(w, `{"id":"txi_old","deleted":true}`)
		case r.Method == "POST" && r.URL.Path == "/v1/customers/cus_1/tax_ids":
			io.WriteString(w, `{"id":"txi_new"}`)
		case r.URL.Path == "/v1/subscriptions/sub_1":
			method := "charge_automatically"
			if form.Get("collection_method") == "send_invoice" {
				method = "send_invoice"
			}
			io.WriteString(w, `{"id":"sub_1","collection_method":"`+method+`","days_until_due":30}`)
		default:
			t.Errorf("unexpected Stripe call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return HTTPStripeClient{BaseURL: srv.URL, APIKey: "sk_test_x", Client: srv.Client()}, &calls
}

func TestStripeBusinessInvoicesDetailsTaxIDAndCollection(t *testing.T) {
	c, calls := fakeStripeBusiness(t)
	ctx := t.Context()
	invoices, more, err := c.ListInvoices(ctx, "cus_1", "", 24)
	if err != nil || !more || len(invoices) != 1 || invoices[0].Tax != 2500 || invoices[0].DueDate == nil || invoices[0].PDFURL == "" {
		t.Fatalf("invoices=%+v more=%v err=%v", invoices, more, err)
	}
	d, err := c.GetBillingDetails(ctx, "cus_1")
	if err != nil || d.Name != "Acme" || d.Address.Country != "DE" || d.TaxID == nil || d.TaxID.Verification != "verified" {
		t.Fatalf("details=%+v err=%v", d, err)
	}
	if err = c.SetTaxID(ctx, "cus_1", &TaxID{Type: "eu_vat", Value: "DE000000000"}, "k"); err != nil {
		t.Fatal(err)
	}
	if err = c.SetTaxID(ctx, "cus_1", &TaxID{Type: "eu_vat", Value: "DE111111111"}, "k2"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*calls, "\n")
	if strings.Count(joined, "DELETE /v1/customers/cus_1/tax_ids/txi_old") != 1 || !strings.Contains(joined, "POST /v1/customers/cus_1/tax_ids type=eu_vat&value=DE111111111") {
		t.Fatalf("tax id calls:\n%s", joined)
	}
	got, err := c.SetCollection(ctx, "sub_1", Collection{PayByInvoice: true, DaysUntilDue: 30}, "k3")
	if err != nil || !got.PayByInvoice || got.DaysUntilDue != 30 || !strings.Contains(strings.Join(*calls, "\n"), "collection_method=send_invoice&days_until_due=30") {
		t.Fatalf("collection=%+v err=%v", got, err)
	}
	if got, err = c.SetCollection(ctx, "sub_1", Collection{}, "k4"); err != nil || got.PayByInvoice {
		t.Fatalf("back to card: %+v err=%v", got, err)
	}
}

func TestBillingDetailsValidation(t *testing.T) {
	good := BillingDetails{Email: "billing@example.test", Name: "Acme GmbH", Address: Address{Line1: "1 Road", City: "Berlin", PostalCode: "10115", Country: "DE"}, TaxID: &TaxID{Type: "eu_vat", Value: "DE123456789"}}
	if !good.validate() {
		t.Fatal("valid details refused")
	}
	for name, edit := range map[string]func(*BillingDetails){
		"no email":            func(d *BillingDetails) { d.Email = "" },
		"bad email":           func(d *BillingDetails) { d.Email = "not an email" },
		"lower-case country":  func(d *BillingDetails) { d.Address.Country = "de" },
		"address, no country": func(d *BillingDetails) { d.Address.Country = "" },
		"newline in name":     func(d *BillingDetails) { d.Name = "Acme\nEvil" },
		"tax type":            func(d *BillingDetails) { d.TaxID.Type = "VAT" },
		"tax value":           func(d *BillingDetails) { d.TaxID.Value = "<script>" },
	} {
		d := good
		tax := *good.TaxID
		d.TaxID = &tax
		edit(&d)
		if d.validate() {
			t.Errorf("%s: accepted", name)
		}
	}
	if !(BillingDetails{Email: "a@example.test"}).validate() {
		t.Fatal("email alone refused")
	}
}

// An overdue pay-by-invoice invoice starts the renewal grace from its due date.
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
