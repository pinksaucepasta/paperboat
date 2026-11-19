package revenue

import "testing"

const now = int64(1_800_000_000)
const day0 = int64(86400)

func monthlyItem(amount, qty int64) Item {
	return Item{PriceID: "price_m", Amount: amount, Quantity: qty, Interval: "month", Count: 1}
}

func TestMRRNormalisesIntervalsAndSeats(t *testing.T) {
	for _, c := range []struct {
		name string
		item Item
		want int64
	}{
		{"monthly seats", monthlyItem(1000, 3), 3000},
		{"yearly: 1200 a year is 100 a month", Item{Amount: 120000, Quantity: 1, Interval: "year", Count: 1}, 10000},
		{"quarterly", Item{Amount: 3000, Quantity: 1, Interval: "month", Count: 3}, 1000},
		{"weekly", Item{Amount: 100, Quantity: 1, Interval: "week", Count: 1}, 433},
		{"metered is not recurring revenue", Item{Amount: 100, Quantity: 5, Interval: "month", Count: 1, Metered: true}, 0},
		{"no seats", monthlyItem(1000, 0), 0},
	} {
		if got := (Sub{Status: "active", Items: []Item{c.item}}).MRR(now); got != c.want {
			t.Errorf("%s: MRR = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMRRCountsOnlyActiveAndPastDue(t *testing.T) {
	for status, want := range map[string]int64{"active": 1000, "past_due": 1000, "canceled": 0, "paused": 0, "unpaid": 0, "incomplete": 0, "incomplete_expired": 0} {
		if got := (Sub{Status: status, Items: []Item{monthlyItem(1000, 1)}}).MRR(now); got != want {
			t.Errorf("%s: MRR = %d, want %d", status, got, want)
		}
	}
}

func TestMRRDiscounts(t *testing.T) {
	yearly := Item{Amount: 120000, Quantity: 1, Interval: "year", Count: 1}
	for _, c := range []struct {
		name  string
		items []Item
		d     []Discount
		want  int64
	}{
		{"forever percent", []Item{monthlyItem(1000, 3)}, []Discount{{Percent: 20, Duration: "forever"}}, 2400},
		{"amount off is per invoice: 5.00 off a yearly invoice is 0.42 a month", []Item{yearly}, []Discount{{AmountOff: 500, Currency: "usd", Duration: "forever"}}, 9958},
		{"once never reduces MRR", []Item{monthlyItem(1000, 1)}, []Discount{{Percent: 50, Duration: "once"}}, 1000},
		{"repeating still running", []Item{monthlyItem(1000, 1)}, []Discount{{Percent: 50, Duration: "repeating", End: now + day0}}, 500},
		{"repeating already ended", []Item{monthlyItem(1000, 1)}, []Discount{{Percent: 50, Duration: "repeating", End: now - 1}}, 1000},
		{"stacked percent then amount", []Item{monthlyItem(1000, 1)}, []Discount{{Percent: 50, Duration: "forever"}, {AmountOff: 100, Currency: "usd", Duration: "forever"}}, 400},
		{"never below zero", []Item{monthlyItem(1000, 1)}, []Discount{{AmountOff: 5000, Currency: "usd", Duration: "forever"}}, 0},
		{"amount off in another currency is ignored", []Item{monthlyItem(1000, 1)}, []Discount{{AmountOff: 500, Currency: "eur", Duration: "forever"}}, 1000},
	} {
		if got := (Sub{Status: "active", Currency: "usd", Items: c.items, Discounts: c.d}).MRR(now); got != c.want {
			t.Errorf("%s: MRR = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCycleAmountIsWhatTheRenewalCharges(t *testing.T) {
	s := Sub{Status: "active", Items: []Item{{Amount: 120000, Quantity: 2, Interval: "year", Count: 1}}, Discounts: []Discount{{Percent: 25, Duration: "forever"}}}
	if got := s.CycleAmount(now); got != 180000 {
		t.Fatalf("CycleAmount = %d, want 180000", got)
	}
}

func TestPrimaryCurrencyAndSplit(t *testing.T) {
	subs := []Sub{
		{ID: "a", Status: "active", Currency: "usd", Items: []Item{monthlyItem(1000, 1)}},
		{ID: "b", Status: "active", Currency: "eur", Items: []Item{monthlyItem(5000, 1)}},
		{ID: "c", Status: "canceled", Currency: "gbp", Items: []Item{monthlyItem(9000, 1)}},
	}
	cur := PrimaryCurrency(subs, now)
	if cur != "eur" {
		t.Fatalf("primary = %s, want eur", cur)
	}
	kept, skipped := SplitCurrency(subs, cur)
	if len(kept) != 1 || skipped != 2 {
		t.Fatalf("kept %d skipped %d", len(kept), skipped)
	}
	if PrimaryCurrency(nil, now) != "usd" {
		t.Fatal("no subscriptions: default usd")
	}
}

func TestTrendRebuildsHistoryFromDates(t *testing.T) {
	subs := []Sub{
		{ID: "old", Status: "active", Start: now - 100*day0, Items: []Item{monthlyItem(1000, 1)}},
		{ID: "new", Status: "active", Start: now - 5*day0, Items: []Item{monthlyItem(2000, 1)}},
		{ID: "gone", Status: "canceled", Start: now - 50*day0, Ended: now - 10*day0, Items: []Item{monthlyItem(4000, 1)}},
	}
	for _, c := range []struct {
		at   int64
		want int64
	}{{now - 60*day0, 1000}, {now - 30*day0, 1000 + 4000}, {now - 15*day0, 1000 + 4000}, {now - 9*day0, 1000}, {now - 3*day0, 1000 + 2000}, {now, 1000 + 2000}} {
		got, _ := MRRAt(subs, c.at, now)
		if got != c.want {
			t.Errorf("MRRAt(now-%dd) = %d, want %d", (now-c.at)/day0, got, c.want)
		}
	}
}

func TestMovementBetween(t *testing.T) {
	from, to := now-30*day0, now
	subs := []Sub{
		{ID: "base", Status: "active", Start: from - 100*day0, Items: []Item{monthlyItem(1000, 1)}},
		{ID: "base2", Status: "active", Start: from - 100*day0, Items: []Item{monthlyItem(3000, 1)}},
		{ID: "new", Status: "active", Start: from + 2*day0, Items: []Item{monthlyItem(2000, 1)}},
		{ID: "churn", Status: "canceled", Start: from - 60*day0, Ended: from + 5*day0, Items: []Item{monthlyItem(4000, 1)}},
		{ID: "unfinished", Status: "incomplete_expired", Start: from + 1*day0, Items: []Item{monthlyItem(6000, 1)}},
	}
	m := MovementBetween(subs, from, to, now)
	// At the start: base, base2 and churn pay (4000+1000+3000 = 8000 across 3).
	if m.StartActive != 3 || m.StartMRR != 8000 {
		t.Errorf("start = %d subscribers, %d MRR; want 3, 8000", m.StartActive, m.StartMRR)
	}
	if m.New != 1 || m.NewMRR != 2000 { // incomplete never counts
		t.Errorf("new = %d (%d MRR), want 1 (2000)", m.New, m.NewMRR)
	}
	if m.Churned != 1 || m.ChurnedMRR != 4000 {
		t.Errorf("churned = %d (%d MRR), want 1 (4000)", m.Churned, m.ChurnedMRR)
	}
	if m.LogoChurnPercent != 33.3 || m.RevenueChurnPercent != 50 {
		t.Errorf("churn = %v%% logos, %v%% revenue; want 33.3, 50", m.LogoChurnPercent, m.RevenueChurnPercent)
	}
}

func TestHistoryUsesTheDiscountsThatRanThen(t *testing.T) {
	s := Sub{ID: "a", Status: "active", Start: now - 90*day0, Items: []Item{monthlyItem(1000, 1)},
		Discounts: []Discount{{Percent: 50, Duration: "repeating", End: now - 2*day0}}}
	if got, _ := MRRAt([]Sub{s}, now-10*day0, now); got != 500 {
		t.Fatalf("10 days ago the coupon still ran: MRR %d, want 500", got)
	}
	if got, _ := MRRAt([]Sub{s}, now-1*day0, now); got != 1000 {
		t.Fatalf("yesterday it had ended: MRR %d, want 1000", got)
	}
}

func TestNoCurrencyIsNeverTheReportCurrency(t *testing.T) {
	subs := []Sub{{Status: "active", Items: []Item{monthlyItem(9000, 1)}}, {Status: "active", Currency: "usd", Items: []Item{monthlyItem(1000, 1)}}}
	cur := PrimaryCurrency(subs, now)
	kept, skipped := SplitCurrency(subs, cur)
	if cur != "usd" || len(kept) != 1 || skipped != 1 {
		t.Fatalf("%q kept %d skipped %d", cur, len(kept), skipped)
	}
}
