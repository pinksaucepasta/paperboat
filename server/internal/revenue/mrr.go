// Package revenue answers the founders' money questions from Stripe: MRR and ARR, who subscribes to what, who is
// failing to pay, what was collected. It reads Stripe on demand (no copy in our database, so nothing to drift), caches
// briefly, and stays optional: without a Stripe key every call returns ErrNotConfigured.
//
// MRR follows Stripe's own definition (https://docs.stripe.com/billing/subscriptions/analytics/glossary and
// https://support.stripe.com/questions/understanding-monthly-recurring-revenue-(mrr)): the sum of every active and
// past_due subscription's recurring price normalised to one month (a 1200 yearly plan is 100), times quantity, after
// the discounts that keep applying; paused, unpaid, canceled and incomplete subscriptions, tax and one-time
// charges are not MRR. One-time ("once") discounts do not reduce MRR (Stripe's default); forever and still-running
// repeating discounts do.
package revenue

import "math"

// Item is one subscription line: a licensed recurring price and a quantity (seats).
type Item struct {
	PriceID  string
	Amount   int64 // unit_amount in the smallest currency unit
	Quantity int64
	Interval string // day, week, month, year
	Count    int64  // interval_count: 3 with month is a quarter
	Metered  bool
}

// Discount is a coupon on a subscription.
type Discount struct {
	Percent   float64
	AmountOff int64 // per invoice
	Currency  string
	Duration  string // forever, repeating, once
	End       int64  // unix; 0 when it never ends
}

// Invoice is the latest invoice of a subscription, as far as dunning needs it.
type Invoice struct {
	ID              string
	Status          string
	AmountRemaining int64
	AttemptCount    int64
	NextAttempt     int64
	Created         int64
}

// Sub is one Stripe subscription reduced to what the money page uses.
type Sub struct {
	ID, Customer, Status, Currency string
	Start, Ended                   int64
	PeriodEnd                      int64
	CancelAtPeriodEnd              bool
	Items                          []Item
	Discounts                      []Discount
	Invoice                        *Invoice
	DiscountUnknown                bool // a discount whose coupon Stripe did not return: this MRR may be too high
}

// counts reports whether a status is part of MRR: Stripe counts active and past_due.
func counts(status string) bool { return status == "active" || status == "past_due" }

// months is how many months one billing period of an item spans.
func months(interval string, count int64) float64 {
	if count < 1 {
		count = 1
	}
	switch interval {
	case "year":
		return 12 * float64(count)
	case "month":
		return float64(count)
	case "week":
		return 12 / 52.0 * float64(count)
	case "day":
		return 12 / 365.0 * float64(count)
	}
	return 0
}

// period is the billing period length in months (items of one subscription share it).
func (s Sub) period() float64 {
	for _, it := range s.Items {
		if m := months(it.Interval, it.Count); !it.Metered && m > 0 {
			return m
		}
	}
	return 0
}

// monthly is the subscription's recurring value per month in the smallest unit, ignoring its status.
func (s Sub) monthly(now int64) float64 {
	var total float64
	for _, it := range s.Items {
		if m := months(it.Interval, it.Count); !it.Metered && m > 0 && it.Quantity > 0 {
			total += float64(it.Amount) * float64(it.Quantity) / m
		}
	}
	p := s.period()
	for _, d := range s.Discounts {
		if d.Duration == "once" || d.Duration == "repeating" && d.End != 0 && d.End <= now {
			continue
		}
		switch {
		case d.Percent > 0:
			total *= 1 - math.Min(d.Percent, 100)/100
		case d.AmountOff > 0 && p > 0 && (d.Currency == "" || d.Currency == s.Currency):
			total -= float64(d.AmountOff) / p
		}
	}
	return math.Max(total, 0)
}

// MRR is the subscription's contribution to MRR right now, in the smallest currency unit.
func (s Sub) MRR(now int64) int64 {
	if !counts(s.Status) {
		return 0
	}
	return int64(math.Round(s.monthly(now)))
}

// CycleAmount is what the subscription's next renewal charges before tax, in the smallest currency unit.
func (s Sub) CycleAmount(now int64) int64 {
	return int64(math.Round(s.monthly(now) * s.period()))
}

// mrrAt rebuilds the subscription's MRR at a past instant from its start and end dates, with the discounts that
// ran then. It values the subscription at today's price and status: a plan or pause since then is not replayed
// (Stripe's own chart has history; we do not).
func (s Sub) mrrAt(t, now int64) int64 {
	switch s.Status {
	case "incomplete", "incomplete_expired", "paused", "unpaid":
		if s.Ended == 0 {
			return 0
		}
	}
	if s.Start == 0 || s.Start > t || s.Ended != 0 && s.Ended <= t {
		return 0
	}
	return int64(math.Round(s.monthly(t)))
}

// TrendPoint is the MRR at the end of one UTC day.
type TrendPoint struct {
	Day string `json:"day"`
	MRR int64  `json:"mrr"`
}

// PrimaryCurrency is the currency holding the most MRR (usd when nothing counts yet); sums are never mixed across
// currencies, so the page reports in this one and says how many subscriptions it left out.
func PrimaryCurrency(subs []Sub, now int64) string {
	by := map[string]int64{}
	for _, s := range subs {
		if s.Currency != "" {
			by[s.Currency] += s.MRR(now)
		}
	}
	best, bestMRR := "usd", int64(0)
	for c, v := range by {
		if v > bestMRR || v == bestMRR && v > 0 && c < best {
			best, bestMRR = c, v
		}
	}
	return best
}

// SplitCurrency keeps the subscriptions in currency and counts the others.
func SplitCurrency(subs []Sub, currency string) (kept []Sub, skipped int) {
	for _, s := range subs {
		if s.Currency == currency {
			kept = append(kept, s)
		} else {
			skipped++
		}
	}
	return kept, skipped
}

// Totals sums MRR over the subscriptions that count.
func Totals(subs []Sub, now int64) (mrr, active int64) {
	for _, s := range subs {
		if counts(s.Status) {
			mrr += s.MRR(now)
			active++
		}
	}
	return mrr, active
}

// MRRAt is the MRR at an instant, for the trend and the previous-period comparison.
func MRRAt(subs []Sub, t, now int64) (mrr int64, active int64) {
	for _, s := range subs {
		if v := s.mrrAt(t, now); v > 0 {
			mrr += v
			active++
		}
	}
	return mrr, active
}

// Movement is what happened to the subscriber base between two instants, from subscription dates alone.
type Movement struct {
	New, Churned          int64
	NewMRR, ChurnedMRR    int64
	StartActive, StartMRR int64
	LogoChurnPercent      float64 // churned / subscribers at the start
	RevenueChurnPercent   float64 // churned MRR / MRR at the start
}

// MovementBetween counts subscriptions that began paying in [from, to) and paying ones that ended in it.
func MovementBetween(subs []Sub, from, to, now int64) Movement {
	var m Movement
	m.StartMRR, m.StartActive = MRRAt(subs, from, now)
	for _, s := range subs {
		switch s.Status {
		case "incomplete", "incomplete_expired":
			continue
		}
		v := int64(math.Round(s.monthly(now)))
		if v == 0 {
			continue
		}
		if p := s.Start; p >= from && p < to && (s.Ended == 0 || s.Ended > p) {
			m.New++
			m.NewMRR += v
		}
		if s.Ended >= from && s.Ended < to && s.Ended > s.Start {
			m.Churned++
			m.ChurnedMRR += v
		}
	}
	if m.StartActive > 0 {
		m.LogoChurnPercent = round1(100 * float64(m.Churned) / float64(m.StartActive))
	}
	if m.StartMRR > 0 {
		m.RevenueChurnPercent = round1(100 * float64(m.ChurnedMRR) / float64(m.StartMRR))
	}
	return m
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
