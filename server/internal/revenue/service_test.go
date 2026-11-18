package revenue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

// fixture serves canned Stripe responses by path; an entry may be an error.
type fixture map[string]any

func (f fixture) Get(_ context.Context, pathWithQuery string, out any) error {
	path, _, _ := strings.Cut(pathWithQuery, "?")
	v, ok := f[path]
	if !ok {
		return fmt.Errorf("unexpected request %s", pathWithQuery)
	}
	if err, isErr := v.(error); isErr {
		return err
	}
	if pageFn, isFn := v.(func(url.Values) any); isFn {
		_, q, _ := strings.Cut(pathWithQuery, "?")
		values, _ := url.ParseQuery(q)
		v = pageFn(values)
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}

type directory struct{}

func (directory) PlanCodes(context.Context) (map[string]string, error) {
	return map[string]string{"price_pro": "pro", "price_team": "team"}, nil
}

func (directory) Workspaces(_ context.Context, customers []string) (map[string]string, error) {
	out := map[string]string{}
	for _, c := range customers {
		out[c] = "ws_" + c
	}
	return out, nil
}

func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func lst(items ...any) map[string]any { return obj("data", items, "has_more", false) }

func sub(id, customer, status string, start int64, price string, amount, qty int64, interval string, extra map[string]any) map[string]any {
	m := obj("id", id, "customer", customer, "status", status, "currency", "usd", "start_date", start,
		"items", obj("data", []any{obj("id", "si_"+id, "quantity", qty, "current_period_end", testNow+5*86400,
			"price", obj("id", price, "unit_amount", amount, "recurring", obj("interval", interval, "interval_count", 1, "usage_type", "licensed")))}))
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var testNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC).Unix()

func newService(f fixture) *Service {
	s := New(f, directory{})
	s.now = func() time.Time { return time.Unix(testNow, 0) }
	return s
}

func baseFixture() fixture {
	d := int64(86400)
	return fixture{
		"/v1/subscriptions": lst(
			sub("sub_a", "cus_a", "active", testNow-90*d, "price_pro", 2000, 1, "month", nil),
			sub("sub_b", "cus_b", "active", testNow-40*d, "price_team", 120000, 2, "year", obj("discounts", []any{obj("end", 0, "coupon", obj("percent_off", 25, "duration", "forever"))})),
			sub("sub_c", "cus_c", "past_due", testNow-60*d, "price_pro", 2000, 1, "month",
				obj("latest_invoice", obj("id", "in_1", "status", "open", "amount_remaining", 2000, "attempt_count", 2, "next_payment_attempt", testNow+86400, "created", testNow-3*d))),
			sub("sub_d", "cus_d", "trialing", testNow-2*d, "price_pro", 2000, 1, "month", obj("trial_end", testNow+5*d)),
			sub("sub_e", "cus_e", "canceled", testNow-80*d, "price_pro", 2000, 1, "month", obj("ended_at", testNow-3*d)),
			sub("sub_f", "cus_f", "active", testNow-3*d, "price_pro", 2000, 1, "month", obj("cancel_at_period_end", true)),
		),
		"/v1/charges": lst(
			obj("id", "ch_1", "amount", 2000, "currency", "usd", "status", "succeeded", "created", testNow-1*d),
			obj("id", "ch_2", "amount", 4500, "currency", "usd", "status", "succeeded", "created", testNow-20*d),
			obj("id", "ch_3", "amount", 2000, "currency", "usd", "status", "failed", "created", testNow-2*d),
			obj("id", "ch_4", "amount", 9999, "currency", "eur", "status", "succeeded", "created", testNow-1*d),
		),
		"/v1/refunds": lst(obj("id", "re_1", "amount", 500, "currency", "usd", "status", "succeeded", "created", testNow-1*d)),
		"/v1/events": lst(
			obj("id", "evt_up", "type", "customer.subscription.updated", "created", testNow-2*d,
				"data", obj("object", sub("sub_a", "cus_a", "active", testNow-90*d, "price_pro", 2000, 3, "month", nil),
					"previous_attributes", obj("items", obj("data", []any{obj("id", "si_sub_a", "quantity", 1)})))),
			obj("id", "evt_down", "type", "customer.subscription.updated", "created", testNow-3*d,
				"data", obj("object", sub("sub_b", "cus_b", "active", testNow-40*d, "price_team", 120000, 1, "year", obj("discounts", []any{obj("end", 0, "coupon", obj("percent_off", 25, "duration", "forever"))})),
					"previous_attributes", obj("items", obj("data", []any{obj("id", "si_sub_b", "quantity", 2)})))),
			obj("id", "evt_other", "type", "customer.subscription.updated", "created", testNow-3*d,
				"data", obj("object", sub("sub_a", "cus_a", "active", testNow-90*d, "price_pro", 2000, 3, "month", nil), "previous_attributes", obj("status", "trialing"))),
		),
		"/v1/balance": obj("available", []any{obj("amount", 12345, "currency", "usd")}, "pending", []any{obj("amount", 600, "currency", "usd")}),
		"/v1/payouts": lst(
			obj("amount", 5000, "currency", "usd", "status", "paid", "arrival_date", testNow-86400),
			obj("amount", 7000, "currency", "usd", "status", "in_transit", "arrival_date", testNow+2*86400),
			obj("amount", 8000, "currency", "usd", "status", "pending", "arrival_date", testNow+4*86400)),
	}
}

func TestReportMath(t *testing.T) {
	r, err := newService(baseFixture()).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	// MRR: a 2000 + b (2 seats x 120000/12 = 20000, 25% off = 15000) + c past_due 2000 + f 2000 = 21000; the trial and the canceled one are out.
	if r.MRR.Value != 21000 || r.ARR.Value != 252000 || r.Subscribers.Value != 4 {
		t.Errorf("MRR %d ARR %d subscribers %d", r.MRR.Value, r.ARR.Value, r.Subscribers.Value)
	}
	// A week ago: a, b, c and e paid (e ended 3 days ago); f started 3 days ago; d is a trial that began 2 days ago.
	if r.MRR.Previous != 2000+15000+2000+2000 || r.Subscribers.Previous != 4 {
		t.Errorf("previous MRR %d subscribers %d", r.MRR.Previous, r.Subscribers.Previous)
	}
	if got := len(r.Trend); got != 8 || r.Trend[7].MRR != 21000 {
		t.Errorf("trend has %d points, last %d", got, r.Trend[len(r.Trend)-1].MRR)
	}
	if r.Movement.New.Count != 1 || r.Movement.New.MRR != 2000 || r.Movement.Churned.Count != 1 || r.Movement.Churned.MRR != 2000 {
		t.Errorf("movement %+v", r.Movement)
	}
	if r.Movement.LogoChurnPercent != 25 { // 1 of the 4 who paid a week ago
		t.Errorf("logo churn = %v", r.Movement.LogoChurnPercent)
	}
	// Seats 1 -> 3 on a 2000 plan is +4000; 2 -> 1 on the team plan is -7500 (15000 -> 7500).
	if u := r.Movement.Upgraded; u.Count != 1 || u.MRR != 4000 || u.PreviousCount == nil {
		t.Errorf("upgraded %+v", u)
	}
	if d := r.Movement.Downgraded; d.Count != 1 || d.MRR != 7500 {
		t.Errorf("downgraded %+v", d)
	}
	if r.Trials.Active != 1 || r.Trials.EndingSoon != 1 || r.Ending != 1 {
		t.Errorf("trials %+v ending %d", r.Trials, r.Ending)
	}
	if c := r.Cash; c == nil || c.Collected.Value != 2000 || c.Collected.Previous != 4500 || c.Refunds.Value != 500 || c.Net.Value != 1500 || c.FailedPayments.Value != 1 {
		t.Errorf("cash %+v", r.Cash)
	}
	if r.Dunning.Count != 1 || r.Dunning.Rows[0].Amount != 2000 || r.Dunning.Rows[0].Workspace != "ws_cus_c" || r.Dunning.Rows[0].Attempts != 2 {
		t.Errorf("dunning %+v", r.Dunning)
	}
	if len(r.Plans) != 2 || r.Plans[0].Plan != "team" || r.Plans[0].Seats != 2 || r.Plans[1].Plan != "pro" || r.Plans[1].Subscriptions != 3 {
		t.Errorf("plans %+v", r.Plans)
	}
	if r.Customers.Total != 4 || r.Customers.Rows[0].Customer != "cus_b" {
		t.Errorf("customers %+v", r.Customers)
	}
	// Renewals: only active subscriptions that are not ending (a, b); c is past due, f stops at period end.
	if r.Renewals.Total != 2 || r.Renewals.Rows[0].Amount != 2000 {
		t.Errorf("renewals %+v", r.Renewals)
	}
	if b := r.Balance; b == nil || b.Available != 12345 || b.Pending != 600 || b.Payout == nil || b.Payout.Amount != 7000 {
		t.Errorf("balance %+v", r.Balance)
	}
	if r.SkippedCurrency != 0 || len(r.Errors) != 0 {
		t.Errorf("skipped %d errors %v", r.SkippedCurrency, r.Errors)
	}
}

func TestThirtyDaysHasNoUpgradeComparison(t *testing.T) {
	r, err := newService(baseFixture()).Report(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if r.Movement.Upgraded.PreviousCount != nil || r.Movement.Downgraded.PreviousCount != nil {
		t.Fatal("30 days cannot compare upgrades: Stripe events stop at 30 days")
	}
	if r.Movement.New.PreviousCount == nil {
		t.Fatal("new subscriptions compare from subscription dates")
	}
}

func TestNotConfiguredIsNotAnError(t *testing.T) {
	s := New(nil, nil)
	if s.Configured() {
		t.Fatal("no getter: not configured")
	}
	if _, err := s.Report(context.Background(), 30); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("report: %v", err)
	}
	if _, err := s.List(context.Background(), "customers", "", 5); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("list: %v", err)
	}
	if _, err := s.Events(context.Background(), "", 5); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("events: %v", err)
	}
	var nilService *Service
	if nilService.Configured() {
		t.Fatal("nil service: not configured")
	}
}

func TestDownStripeIsAProviderErrorNotNotConfigured(t *testing.T) {
	f := baseFixture()
	f["/v1/subscriptions"] = billing.StripeAPIError{StatusCode: 503}
	_, err := newService(f).Report(context.Background(), 30)
	if !errors.Is(err, ErrProvider) || errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestOneSectionDownKeepsTheRest(t *testing.T) {
	f := baseFixture()
	f["/v1/balance"] = billing.StripeAPIError{StatusCode: 403}
	f["/v1/charges"] = billing.StripeAPIError{StatusCode: 429}
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if r.Balance != nil || r.Errors["balance_and_payouts"] != "unauthorized" || r.Cash != nil || r.Errors["revenue"] != "rate_limited" {
		t.Fatalf("balance %+v cash %+v errors %v", r.Balance, r.Cash, r.Errors)
	}
	if r.MRR.Value != 21000 {
		t.Fatalf("MRR must survive a failing section: %d", r.MRR.Value)
	}
}

func TestListsPage(t *testing.T) {
	s := newService(baseFixture())
	page, err := s.List(context.Background(), "customers", "", 2)
	if err != nil || len(page.Rows) != 2 || page.Total != 4 || page.Next != "2" {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	page, err = s.List(context.Background(), "customers", page.Next, 2)
	if err != nil || len(page.Rows) != 2 || page.Next != "" {
		t.Fatalf("page 2: %+v %v", page, err)
	}
	for _, bad := range [][2]string{{"nope", ""}, {"customers", "-1"}, {"customers", "x"}} {
		if _, err := s.List(context.Background(), bad[0], bad[1], 5); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	trials, _ := s.List(context.Background(), "trials", "", 5)
	if trials.Total != 1 || trials.Rows[0].At != testNow+5*86400 {
		t.Fatalf("trials %+v", trials)
	}
}

func TestEventsPage(t *testing.T) {
	f := fixture{"/v1/events": func(q url.Values) any {
		if q.Get("starting_after") != "evt_0" || q.Get("limit") != "2" {
			t.Errorf("query %v", q)
		}
		return obj("has_more", true, "data", []any{
			obj("id", "evt_1", "type", "invoice.paid", "created", 1, "data", obj("object", obj("id", "in_1"))),
			obj("id", "evt_2", "type", "charge.failed", "created", 2, "data", obj("object", obj("id", "ch_1")))})
	}}
	page, err := newService(f).Events(context.Background(), "evt_0", 2)
	if err != nil || len(page.Events) != 2 || page.Next != "evt_2" || page.Events[0].ObjectID != "in_1" {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestOtherCurrenciesAreLeftOutAndCounted(t *testing.T) {
	f := baseFixture()
	eur := sub("sub_eur", "cus_eur", "active", testNow-10*86400, "price_pro", 100, 1, "month", obj("currency", "eur"))
	f["/v1/subscriptions"] = lst(sub("sub_a", "cus_a", "active", testNow-90*86400, "price_pro", 2000, 1, "month", nil), eur)
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if r.Currency != "usd" || r.SkippedCurrency != 1 || r.MRR.Value != 2000 {
		t.Fatalf("%s skipped %d MRR %d", r.Currency, r.SkippedCurrency, r.MRR.Value)
	}
}

func TestParsesUnexpandedAndExpandedObjects(t *testing.T) {
	raw := `{"id":"sub_1","customer":{"id":"cus_1"},"status":"active","currency":"USD","start_date":5,
	"latest_invoice":"in_9","discounts":["di_1",{"end":0,"source":{"type":"coupon","coupon":{"percent_off":10,"duration":"forever"}}}],
	"items":{"data":[{"id":"si_1","quantity":2,"current_period_end":99,"price":{"id":"price_x","unit_amount":1000,"recurring":{"interval":"month","interval_count":1,"usage_type":"licensed"}}},
	{"id":"si_2","price":{"id":"price_t","unit_amount":null,"recurring":{"interval":"month","interval_count":1}}}]}}`
	var w wireSub
	if err := decodeStrict([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	s := w.sub()
	if s.Customer != "cus_1" || s.Currency != "usd" || s.Invoice != nil || len(s.Items) != 1 || len(s.Discounts) != 1 || s.PeriodEnd != 99 {
		t.Fatalf("%+v", s)
	}
	if got := s.MRR(testNow); got != 1800 {
		t.Fatalf("MRR %d, want 1800 (2 x 1000 less 10%%; the tiered line has no unit amount)", got)
	}
}

func TestUpgradesInOtherCurrenciesAreNotSummed(t *testing.T) {
	f := baseFixture()
	eur := sub("sub_eur", "cus_eur", "active", testNow-90*86400, "price_pro", 2000, 3, "month", obj("currency", "eur"))
	f["/v1/events"] = lst(obj("id", "evt_eur", "type", "customer.subscription.updated", "created", testNow-86400,
		"data", obj("object", eur, "previous_attributes", obj("items", obj("data", []any{obj("id", "si_sub_eur", "quantity", 1)})))))
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if r.Movement.Upgraded.Count != 0 || r.Movement.Upgraded.MRR != 0 {
		t.Fatalf("a EUR upgrade leaked into the USD report: %+v", r.Movement.Upgraded)
	}
}

func TestAListObjectWithoutAnIdIsAProviderErrorNotALoop(t *testing.T) {
	f := baseFixture()
	f["/v1/charges"] = obj("has_more", true, "data", []any{obj("amount", 1, "currency", "usd", "status", "succeeded", "created", testNow)})
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if r.Cash != nil || r.Errors["revenue"] == "" {
		t.Fatalf("cash %+v errors %v", r.Cash, r.Errors)
	}
}

func TestACutOffReadMarksTheReportPartial(t *testing.T) {
	f := baseFixture()
	page := func(url.Values) any {
		return obj("has_more", true, "data", []any{obj("id", "ch_x", "amount", 1, "currency", "usd", "status", "succeeded", "created", testNow-86400)})
	}
	f["/v1/charges"] = page
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Partial {
		t.Fatal("charges ran past the read bound: the report must say its sums are a floor")
	}
}

func TestNilDirectoryKnowsNothingInsteadOfPanicking(t *testing.T) {
	s := New(baseFixture(), nil)
	s.now = func() time.Time { return time.Unix(testNow, 0) }
	if _, err := s.Report(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
}

// Captured from the Stripe sandbox (API 2026-09-30): a discount's coupon is a bare id unless source.coupon is expanded.
func TestABareCouponIdIsUnknownNotAParseFailure(t *testing.T) {
	raw := `{"id":"sub_1","customer":"cus_1","status":"active","currency":"usd","start_date":5,
	"discounts":[{"id":"di_1","object":"discount","end":null,"source":{"coupon":"0C5d7UkF","type":"coupon"},"start":9}],
	"items":{"data":[{"id":"si_1","quantity":1,"current_period_end":99,"price":{"id":"price_x","unit_amount":15000,"recurring":{"interval":"year","interval_count":1,"usage_type":"licensed"}}}]}}`
	var w wireSub
	if err := decodeStrict([]byte(raw), &w); err != nil {
		t.Fatalf("a real sandbox subscription must parse: %v", err)
	}
	if s := w.sub(); !s.DiscountUnknown || len(s.Discounts) != 0 {
		t.Fatalf("%+v", s)
	}
	f := baseFixture()
	f["/v1/subscriptions"] = lst(json.RawMessage(raw))
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil || !r.Partial {
		t.Fatalf("a discount we cannot value must mark the report as a floor: %v partial=%v", err, r.Partial)
	}
}

// Real events carry discount ids, not objects: a discounted subscription is valued with the discounts the list returned.
func TestUpgradeOfADiscountedSubscriptionUsesItsListedDiscount(t *testing.T) {
	d := int64(86400)
	listed := sub("sub_d", "cus_d", "active", testNow-90*d, "price_pro", 2000, 2, "month", obj("discounts", []any{obj("end", nil, "source", obj("type", "coupon", "coupon", obj("percent_off", 50, "duration", "forever")))}))
	event := sub("sub_d", "cus_d", "active", testNow-90*d, "price_pro", 2000, 2, "month", obj("discounts", []any{"di_1"}))
	f := baseFixture()
	f["/v1/subscriptions"] = lst(listed)
	f["/v1/events"] = lst(obj("id", "evt_1", "type", "customer.subscription.updated", "created", testNow-d,
		"data", obj("object", event, "previous_attributes", obj("items", obj("data", []any{obj("id", "si_sub_d", "quantity", 1)})))))
	r, err := newService(f).Report(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	// 1 -> 2 seats of 2000 at 50% off is +1000, not +2000.
	if u := r.Movement.Upgraded; u.Count != 1 || u.MRR != 1000 {
		t.Fatalf("upgraded %+v", u)
	}
}
