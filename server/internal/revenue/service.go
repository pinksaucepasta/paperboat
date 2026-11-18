package revenue

import (
	"context"
	"errors"
	"math"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

var (
	// ErrNotConfigured: no Stripe key on this server. Not a fault; the page says so in one line.
	ErrNotConfigured = errors.New("stripe is not configured")
	// ErrInvalid: a request outside the fixed choices.
	ErrInvalid = errors.New("invalid money request")
	// ErrProvider wraps a Stripe failure of the core subscription read; the HTTP layer answers one safe 503.
	ErrProvider = errors.New("stripe read failed")
)

const (
	cacheTTL      = 2 * time.Minute
	refreshBudget = 45 * time.Second
	previewRows   = 5
	renewalWindow = 30 * 24 * time.Hour
	trialSoon     = 7 * 24 * time.Hour
	day           = 24 * time.Hour
)

// Days are the windows an operator may ask for. Stripe keeps events for 30 days, so 7 can compare against the 7
// before it and 30 cannot (its upgrades and downgrades have no previous value).
var Days = []int{7, 30}

// Directory maps Stripe ids onto our own records.
type Directory interface {
	PlanCodes(ctx context.Context) (map[string]string, error)                      // price id -> plan code
	Workspaces(ctx context.Context, customers []string) (map[string]string, error) // customer id -> workspace id
}

type Service struct {
	g   Getter
	dir Directory
	now func() time.Time
	mu  sync.Mutex
	st  *state
	per map[int]*period
	bal *balance
}

// New returns a service; a nil getter makes every call answer ErrNotConfigured.
func New(g Getter, dir Directory) *Service {
	return &Service{g: g, dir: dir, now: time.Now, per: map[int]*period{}}
}

// Configured reports whether a Stripe key is set.
func (s *Service) Configured() bool { return s != nil && s.g != nil }

// Row is one subscriber in a list. Which fields mean what depends on the list (see the API description).
type Row struct {
	Customer  string `json:"customer"`
	Workspace string `json:"workspace,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Seats     int64  `json:"seats"`
	MRR       int64  `json:"mrr"`
	State     string `json:"state,omitempty"`
	Amount    int64  `json:"amount,omitempty"`
	Attempts  int64  `json:"attempts,omitempty"`
	At        int64  `json:"at,omitempty"`
}

type state struct {
	at       time.Time
	subs     []Sub
	partial  bool
	currency string
	skipped  int
	plans    map[string]string
	ws       map[string]string
}

type period struct {
	at          time.Time
	cur, prev   Cash
	cashErr     string
	up, upPrev  Change
	changesErr  string
	hasPrevious bool
}

type balance struct {
	at  time.Time
	val Balance
	err string
}

func (s *Service) fresh(at time.Time) bool { return s.now().Sub(at) < cacheTTL }

// load returns the subscription state, reading Stripe when the cached one is old.
func (s *Service) load(ctx context.Context) (*state, error) {
	if s.st != nil && s.fresh(s.st.at) {
		return s.st, nil
	}
	subs, partial, err := Subscriptions(ctx, s.g)
	if err != nil {
		return nil, errors.Join(ErrProvider, err)
	}
	now := s.now().Unix()
	cur := PrimaryCurrency(subs, now)
	kept, skipped := SplitCurrency(subs, cur)
	plans, err := s.dir.PlanCodes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var customers []string
	for _, sub := range kept {
		if !seen[sub.Customer] {
			seen[sub.Customer] = true
			customers = append(customers, sub.Customer)
		}
	}
	ws, err := s.dir.Workspaces(ctx, customers)
	if err != nil {
		return nil, err
	}
	s.st = &state{at: s.now(), subs: kept, partial: partial, currency: cur, skipped: skipped, plans: plans, ws: ws}
	return s.st, nil
}

func (st *state) plan(sub Sub) string {
	if len(sub.Items) == 0 {
		return ""
	}
	if code := st.plans[sub.Items[0].PriceID]; code != "" {
		return code
	}
	return sub.Items[0].PriceID
}

func seats(sub Sub) (n int64) {
	for _, it := range sub.Items {
		n += it.Quantity
	}
	return n
}

func (st *state) row(sub Sub, now int64) Row {
	return Row{Customer: sub.Customer, Workspace: st.ws[sub.Customer], Plan: st.plan(sub), Seats: seats(sub), MRR: sub.MRR(now), State: sub.Status}
}

// Lists names the paged lists behind the cards.
var Lists = []string{"customers", "past_due", "renewals", "trials"}

// rows builds one full list, ordered the way its card reads.
func (st *state) rows(kind string, now int64) []Row {
	var out []Row
	switch kind {
	case "customers":
		by := map[string]*Row{}
		for _, sub := range st.subs {
			if !counts(sub.Status) {
				continue
			}
			r := st.row(sub, now)
			r.At = sub.Start
			if have := by[sub.Customer]; have != nil {
				have.MRR += r.MRR
				have.Seats += r.Seats
				continue
			}
			by[sub.Customer] = &r
		}
		for _, r := range by {
			out = append(out, *r)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].MRR > out[j].MRR || out[i].MRR == out[j].MRR && out[i].Customer < out[j].Customer
		})
	case "past_due":
		for _, sub := range st.subs {
			if sub.Status != "past_due" && sub.Status != "unpaid" {
				continue
			}
			r := st.row(sub, now)
			r.MRR = int64(math.Round(sub.monthly(now)))
			if i := sub.Invoice; i != nil {
				r.Amount, r.Attempts, r.At = i.AmountRemaining, i.AttemptCount, i.NextAttempt
			}
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].Amount > out[j].Amount || out[i].Amount == out[j].Amount && out[i].Customer < out[j].Customer
		})
	case "renewals":
		end := now + int64(renewalWindow/time.Second)
		for _, sub := range st.subs {
			if sub.Status != "active" || sub.CancelAtPeriodEnd || sub.PeriodEnd < now || sub.PeriodEnd > end {
				continue
			}
			r := st.row(sub, now)
			r.Amount, r.At = sub.CycleAmount(now), sub.PeriodEnd
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].At < out[j].At || out[i].At == out[j].At && out[i].Customer < out[j].Customer
		})
	case "trials":
		for _, sub := range st.subs {
			if sub.Status != "trialing" {
				continue
			}
			r := st.row(sub, now)
			r.MRR, r.At = int64(math.Round(sub.monthly(now))), sub.TrialEnd
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].At < out[j].At || out[i].At == out[j].At && out[i].Customer < out[j].Customer
		})
	}
	return out
}

// RowPage is one page of a list; Next is the offset of the page after it.
type RowPage struct {
	Rows  []Row  `json:"rows"`
	Total int    `json:"total"`
	Next  string `json:"next_after,omitempty"`
}

// List returns one page of a list by offset.
func (s *Service) List(ctx context.Context, kind, after string, limit int) (RowPage, error) {
	if !s.Configured() {
		return RowPage{}, ErrNotConfigured
	}
	if !slices.Contains(Lists, kind) || limit < 1 || limit > 100 {
		return RowPage{}, ErrInvalid
	}
	offset := 0
	if after != "" {
		n, err := strconv.Atoi(after)
		if err != nil || n < 0 {
			return RowPage{}, ErrInvalid
		}
		offset = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	st, err := s.load(ctx)
	if err != nil {
		return RowPage{}, err
	}
	all := st.rows(kind, s.now().Unix())
	page := RowPage{Total: len(all), Rows: []Row{}}
	if offset < len(all) {
		end := min(offset+limit, len(all))
		page.Rows = all[offset:end]
		if end < len(all) {
			page.Next = strconv.Itoa(end)
		}
	}
	return page, nil
}

// Events reads one page of Stripe's event log (not cached: it is the live view).
func (s *Service) Events(ctx context.Context, cursor string, limit int) (EventPage, error) {
	if !s.Configured() {
		return EventPage{}, ErrNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page, err := Events(ctx, s.g, cursor, limit)
	if err != nil {
		return EventPage{}, errors.Join(ErrProvider, err)
	}
	return page, nil
}

// Report is the money page. Amounts are in the smallest unit of Currency; times are unix seconds.
type Report struct {
	ObservedAt      time.Time         `json:"observed_at"`
	Days            int               `json:"days"`
	Currency        string            `json:"currency"`
	Partial         bool              `json:"partial"`          // more subscriptions than the read bound: every sum is a floor
	SkippedCurrency int               `json:"skipped_currency"` // subscriptions in other currencies, left out of every sum
	MRR             Pair              `json:"mrr"`
	ARR             Pair              `json:"arr"`
	Subscribers     Pair              `json:"subscribers"`
	Trend           []TrendPoint      `json:"trend"`
	Plans           []PlanRow         `json:"plans"`
	Movement        MovementReport    `json:"movement"`
	Trials          TrialsReport      `json:"trials"`
	Cash            *CashReport       `json:"cash,omitempty"`
	Dunning         DunningReport     `json:"dunning"`
	Customers       RowList           `json:"customers"`
	Renewals        RowList           `json:"renewals"`
	Ending          int64             `json:"ending"` // active subscriptions that stop at the end of their period
	Balance         *BalanceReport    `json:"balance,omitempty"`
	Errors          map[string]string `json:"errors,omitempty"`
}

type Pair struct {
	Value    int64 `json:"value"`
	Previous int64 `json:"previous"`
}

type PlanRow struct {
	Plan          string `json:"plan"`
	Subscriptions int64  `json:"subscriptions"`
	Seats         int64  `json:"seats"`
	MRR           int64  `json:"mrr"`
}

// Flow is a count and the MRR it moved; the previous values are absent when Stripe cannot say (events reach back 30 days).
type Flow struct {
	Count         int64  `json:"count"`
	MRR           int64  `json:"mrr"`
	PreviousCount *int64 `json:"previous_count,omitempty"`
	PreviousMRR   *int64 `json:"previous_mrr,omitempty"`
}

type MovementReport struct {
	New                         Flow    `json:"new"`
	Churned                     Flow    `json:"churned"`
	Upgraded                    Flow    `json:"upgraded"`
	Downgraded                  Flow    `json:"downgraded"`
	LogoChurnPercent            float64 `json:"logo_churn_percent"`
	PreviousLogoChurnPercent    float64 `json:"previous_logo_churn_percent"`
	RevenueChurnPercent         float64 `json:"revenue_churn_percent"`
	PreviousRevenueChurnPercent float64 `json:"previous_revenue_churn_percent"`
	ChangesUnavailable          bool    `json:"changes_unavailable"`
}

type TrialsReport struct {
	Active     int64 `json:"active"`
	EndingSoon int64 `json:"ending_soon"`
	Started    Pair  `json:"started"`
}

type CashReport struct {
	Collected      Pair `json:"collected"`
	Refunds        Pair `json:"refunds"`
	Net            Pair `json:"net"`
	FailedPayments Pair `json:"failed_payments"`
}

type DunningReport struct {
	Count     int64 `json:"count"`
	AtRiskMRR int64 `json:"at_risk_mrr"`
	Rows      []Row `json:"rows"`
}

type RowList struct {
	Total int   `json:"total"`
	Rows  []Row `json:"rows"`
}

type BalanceReport struct {
	Available int64   `json:"available"`
	Pending   int64   `json:"pending"`
	Payout    *Payout `json:"next_payout,omitempty"`
}

// Report reads (or reuses) Stripe data and computes the page for a window.
func (s *Service) Report(ctx context.Context, days int) (Report, error) {
	if !s.Configured() {
		return Report{}, ErrNotConfigured
	}
	if !slices.Contains(Days, days) {
		return Report{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	st, err := s.load(ctx)
	if err != nil {
		return Report{}, err
	}
	now := s.now()
	nowU := now.Unix()
	r := Report{ObservedAt: now.UTC(), Days: days, Currency: st.currency, Partial: st.partial, SkippedCurrency: st.skipped, Errors: map[string]string{}}
	span := int64(days) * 86400
	from, prevFrom := nowU-span, nowU-2*span

	r.MRR.Value, r.Subscribers.Value = Totals(st.subs, nowU)
	r.MRR.Previous, r.Subscribers.Previous = MRRAt(st.subs, from, nowU)
	r.ARR = Pair{r.MRR.Value * 12, r.MRR.Previous * 12}
	r.Trend = trend(st.subs, days, now)
	r.Plans = plans(st, nowU)
	for _, sub := range st.subs {
		if sub.Status == "active" && sub.CancelAtPeriodEnd {
			r.Ending++
		}
	}

	cur, prev := MovementBetween(st.subs, from, nowU, nowU), MovementBetween(st.subs, prevFrom, from, nowU)
	r.Movement.New = Flow{Count: cur.New, MRR: cur.NewMRR, PreviousCount: &prev.New, PreviousMRR: &prev.NewMRR}
	r.Movement.Churned = Flow{Count: cur.Churned, MRR: cur.ChurnedMRR, PreviousCount: &prev.Churned, PreviousMRR: &prev.ChurnedMRR}
	r.Movement.LogoChurnPercent, r.Movement.PreviousLogoChurnPercent = cur.LogoChurnPercent, prev.LogoChurnPercent
	r.Movement.RevenueChurnPercent, r.Movement.PreviousRevenueChurnPercent = cur.RevenueChurnPercent, prev.RevenueChurnPercent
	r.Trials.Started = Pair{cur.TrialsStarted, prev.TrialsStarted}
	for _, sub := range st.subs {
		if sub.Status == "trialing" {
			r.Trials.Active++
			if sub.TrialEnd > nowU && sub.TrialEnd-nowU <= int64(trialSoon/time.Second) {
				r.Trials.EndingSoon++
			}
		}
	}

	p := s.period(ctx, st, days, now)
	r.Movement.Upgraded = Flow{Count: p.up.Upgraded, MRR: p.up.UpgradedMRR}
	r.Movement.Downgraded = Flow{Count: p.up.Downgraded, MRR: p.up.DowngradedMRR}
	if p.changesErr != "" {
		r.Errors["upgrades_and_downgrades"] = p.changesErr
		r.Movement.ChangesUnavailable = true
	} else if p.hasPrevious {
		r.Movement.Upgraded.PreviousCount, r.Movement.Upgraded.PreviousMRR = &p.upPrev.Upgraded, &p.upPrev.UpgradedMRR
		r.Movement.Downgraded.PreviousCount, r.Movement.Downgraded.PreviousMRR = &p.upPrev.Downgraded, &p.upPrev.DowngradedMRR
	}
	if p.cashErr != "" {
		r.Errors["revenue"] = p.cashErr
	} else {
		r.Cash = &CashReport{
			Collected:      Pair{p.cur.Collected, p.prev.Collected},
			Refunds:        Pair{p.cur.Refunded, p.prev.Refunded},
			Net:            Pair{p.cur.Net(), p.prev.Net()},
			FailedPayments: Pair{p.cur.FailedPayments, p.prev.FailedPayments},
		}
	}

	dunning := st.rows("past_due", nowU)
	r.Dunning = DunningReport{Count: int64(len(dunning)), Rows: head(dunning)}
	for _, d := range dunning {
		r.Dunning.AtRiskMRR += d.MRR
	}
	customers, renewals := st.rows("customers", nowU), st.rows("renewals", nowU)
	r.Customers = RowList{Total: len(customers), Rows: head(customers)}
	r.Renewals = RowList{Total: len(renewals), Rows: head(renewals)}

	if b := s.balance(ctx, st.currency, now); b.err != "" {
		r.Errors["balance_and_payouts"] = b.err
	} else {
		r.Balance = &BalanceReport{Available: b.val.Available, Pending: b.val.Pending, Payout: b.val.Payout}
	}
	return r, nil
}

func head(rows []Row) []Row {
	if rows == nil {
		return []Row{}
	}
	return rows[:min(len(rows), previewRows)]
}

func trend(subs []Sub, days int, now time.Time) []TrendPoint {
	out := make([]TrendPoint, 0, days+1)
	for i := days; i >= 0; i-- {
		at := now.UTC().Add(-time.Duration(i) * day)
		end := time.Date(at.Year(), at.Month(), at.Day(), 23, 59, 59, 0, time.UTC)
		t := min(end.Unix(), now.Unix())
		v, _ := MRRAt(subs, t, now.Unix())
		if i == 0 {
			v, _ = Totals(subs, now.Unix())
		}
		out = append(out, TrendPoint{Day: at.Format("2006-01-02"), MRR: v})
	}
	return out
}

func plans(st *state, now int64) []PlanRow {
	by := map[string]*PlanRow{}
	for _, sub := range st.subs {
		if !counts(sub.Status) {
			continue
		}
		k := st.plan(sub)
		if by[k] == nil {
			by[k] = &PlanRow{Plan: k}
		}
		by[k].Subscriptions++
		by[k].Seats += seats(sub)
		by[k].MRR += sub.MRR(now)
	}
	out := make([]PlanRow, 0, len(by))
	for _, p := range by {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].MRR > out[j].MRR || out[i].MRR == out[j].MRR && out[i].Plan < out[j].Plan
	})
	return out
}

// period reads the cash and the plan changes of a window, each section failing on its own.
func (s *Service) period(ctx context.Context, st *state, days int, now time.Time) *period {
	if p := s.per[days]; p != nil && s.fresh(p.at) {
		return p
	}
	span := int64(days) * 86400
	from, since := now.Unix()-span, now.Unix()-2*span
	p := &period{at: now}
	var err error
	if p.cur, p.prev, err = CashSince(ctx, s.g, st.currency, since, from); err != nil {
		p.cashErr = failureCode(err)
	}
	// Events reach back 30 days: a window of 7 compares with the 7 before; a window of 30 has nothing to compare with.
	eventsSince := since
	p.hasPrevious = 2*span <= 30*86400
	if !p.hasPrevious {
		eventsSince = from
	}
	if p.up, p.upPrev, err = ChangesSince(ctx, s.g, eventsSince, from, now.Unix()); err != nil {
		p.changesErr = failureCode(err)
	}
	s.per[days] = p
	return p
}

func (s *Service) balance(ctx context.Context, currency string, now time.Time) *balance {
	if s.bal != nil && s.fresh(s.bal.at) {
		return s.bal
	}
	b := &balance{at: now}
	val, err := ReadBalance(ctx, s.g, currency)
	if err != nil {
		b.err = failureCode(err)
	}
	b.val = val
	s.bal = b
	return b
}
