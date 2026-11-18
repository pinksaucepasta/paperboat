package revenue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Getter reads Stripe; billing.HTTPStripeClient is the real one and tests supply fixtures.
type Getter interface {
	Get(ctx context.Context, pathWithQuery string, out any) error
}

const (
	pageLimit = 50  // keeps one response well under the client's 1 MiB cap even with expanded invoices
	maxPages  = 40  // 2000 objects per list; beyond that the report says it is partial
	maxEvents = 600 // Stripe keeps 30 days of events; 12 pages bound the work
)

type list struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
}

// walk follows starting_after through a list, handing each object to fn, and reports whether it stopped early.
func walk(ctx context.Context, g Getter, path string, q url.Values, max int, fn func(json.RawMessage) (id string, err error)) (partial bool, err error) {
	after := ""
	seen := 0
	for page := 0; page < maxPages; page++ {
		v := url.Values{}
		for k, vs := range q {
			v[k] = vs
		}
		v.Set("limit", strconv.Itoa(pageLimit))
		if after != "" {
			v.Set("starting_after", after)
		}
		var out list
		if err := g.Get(ctx, path+"?"+v.Encode(), &out); err != nil {
			return false, err
		}
		for _, raw := range out.Data {
			id, err := fn(raw)
			if err != nil {
				return false, err
			}
			if id == "" {
				return false, errors.New("stripe list object without an id")
			}
			after = id
			seen++
			if seen >= max {
				return out.HasMore || seen < len(out.Data), nil
			}
		}
		if !out.HasMore || len(out.Data) == 0 {
			return false, nil
		}
	}
	return true, nil
}

type wireCoupon struct {
	PercentOff float64 `json:"percent_off"`
	AmountOff  int64   `json:"amount_off"`
	Currency   string  `json:"currency"`
	Duration   string  `json:"duration"`
}

type wireDiscount struct {
	End    int64       `json:"end"`
	Coupon *wireCoupon `json:"coupon"`
	Source *struct {
		Coupon *wireCoupon `json:"coupon"`
	} `json:"source"`
}

// UnmarshalJSON accepts an unexpanded discount (a bare id string) as "no known discount".
func (d *wireDiscount) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return nil
	}
	type plain wireDiscount
	return json.Unmarshal(b, (*plain)(d))
}

type wireInvoice struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	AmountRemaining int64  `json:"amount_remaining"`
	AttemptCount    int64  `json:"attempt_count"`
	NextAttempt     int64  `json:"next_payment_attempt"`
	Created         int64  `json:"created"`
}

func (i *wireInvoice) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return nil
	}
	type plain wireInvoice
	return json.Unmarshal(b, (*plain)(i))
}

type wireItem struct {
	ID               string `json:"id"`
	Quantity         *int64 `json:"quantity"`
	CurrentPeriodEnd int64  `json:"current_period_end"`
	Price            *struct {
		ID         string `json:"id"`
		UnitAmount *int64 `json:"unit_amount"`
		Recurring  *struct {
			Interval      string `json:"interval"`
			IntervalCount int64  `json:"interval_count"`
			UsageType     string `json:"usage_type"`
		} `json:"recurring"`
	} `json:"price"`
}

type wireSub struct {
	ID                string          `json:"id"`
	Customer          json.RawMessage `json:"customer"`
	Status            string          `json:"status"`
	Currency          string          `json:"currency"`
	StartDate         int64           `json:"start_date"`
	EndedAt           int64           `json:"ended_at"`
	TrialEnd          int64           `json:"trial_end"`
	CancelAtPeriodEnd bool            `json:"cancel_at_period_end"`
	Items             struct {
		Data []wireItem `json:"data"`
	} `json:"items"`
	Discounts     []wireDiscount `json:"discounts"`
	Discount      *wireDiscount  `json:"discount"`
	LatestInvoice *wireInvoice   `json:"latest_invoice"`
}

// customerID reads a customer that is either an id or an expanded object.
func customerID(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var obj struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.ID
}

func (w wireItem) item() (Item, bool) {
	if w.Price == nil || w.Price.UnitAmount == nil || w.Price.Recurring == nil {
		return Item{}, false // tiered, custom or one-time prices have no unit amount to normalise
	}
	q := int64(1)
	if w.Quantity != nil {
		q = *w.Quantity
	}
	r := w.Price.Recurring
	return Item{PriceID: w.Price.ID, Amount: *w.Price.UnitAmount, Quantity: q, Interval: r.Interval, Count: r.IntervalCount, Metered: r.UsageType == "metered"}, true
}

func (w wireSub) sub() Sub {
	s := Sub{ID: w.ID, Customer: customerID(w.Customer), Status: w.Status, Currency: strings.ToLower(w.Currency), Start: w.StartDate, Ended: w.EndedAt, TrialEnd: w.TrialEnd, CancelAtPeriodEnd: w.CancelAtPeriodEnd}
	for _, it := range w.Items.Data {
		if item, ok := it.item(); ok {
			s.Items = append(s.Items, item)
		}
		if it.CurrentPeriodEnd != 0 && (s.PeriodEnd == 0 || it.CurrentPeriodEnd < s.PeriodEnd) {
			s.PeriodEnd = it.CurrentPeriodEnd
		}
	}
	ds := w.Discounts
	if w.Discount != nil {
		ds = append(ds, *w.Discount)
	}
	for _, d := range ds {
		c := d.Coupon
		if c == nil && d.Source != nil {
			c = d.Source.Coupon
		}
		if c != nil {
			s.Discounts = append(s.Discounts, Discount{Percent: c.PercentOff, AmountOff: c.AmountOff, Currency: strings.ToLower(c.Currency), Duration: c.Duration, End: d.End})
		}
	}
	if i := w.LatestInvoice; i != nil && i.ID != "" {
		s.Invoice = &Invoice{ID: i.ID, Status: i.Status, AmountRemaining: i.AmountRemaining, AttemptCount: i.AttemptCount, NextAttempt: i.NextAttempt, Created: i.Created}
	}
	return s
}

func decodeStrict(raw []byte, out any) error {
	return json.NewDecoder(bytes.NewReader(raw)).Decode(out)
}

// Subscriptions reads every subscription of any status, with discounts and the latest invoice expanded.
func Subscriptions(ctx context.Context, g Getter) (subs []Sub, partial bool, err error) {
	q := url.Values{"status": {"all"}, "expand[]": {"data.discounts", "data.latest_invoice"}}
	partial, err = walk(ctx, g, "/v1/subscriptions", q, maxPages*pageLimit, func(raw json.RawMessage) (string, error) {
		var w wireSub
		if err := decodeStrict(raw, &w); err != nil {
			return "", err
		}
		subs = append(subs, w.sub())
		return w.ID, nil
	})
	return subs, partial, err
}
