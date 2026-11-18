package revenue

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

// Cash is what was collected and refunded in one window, in the report currency.
type Cash struct {
	Collected, Refunded, FailedPayments int64
}

func (c Cash) Net() int64 { return c.Collected - c.Refunded }

type wireCharge struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	Created  int64  `json:"created"`
}

type wireRefund struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	Created  int64  `json:"created"`
}

// CashSince reads charges and refunds created since `since` (twice the window, so the caller can split it) and
// returns the totals of the window [from, now) and of [since, from).
func CashSince(ctx context.Context, g Getter, currency string, since, from int64) (current, previous Cash, err error) {
	q := url.Values{"created[gte]": {strconv.FormatInt(since, 10)}}
	bucket := func(at int64) *Cash {
		if at >= from {
			return &current
		}
		return &previous
	}
	if _, err = walk(ctx, g, "/v1/charges", q, maxPages*pageLimit, func(raw json.RawMessage) (string, error) {
		var c wireCharge
		if err := decodeStrict(raw, &c); err != nil {
			return "", err
		}
		if strings.ToLower(c.Currency) == currency {
			switch c.Status {
			case "succeeded":
				bucket(c.Created).Collected += c.Amount
			case "failed":
				bucket(c.Created).FailedPayments++
			}
		}
		return c.ID, nil
	}); err != nil {
		return Cash{}, Cash{}, err
	}
	_, err = walk(ctx, g, "/v1/refunds", q, maxPages*pageLimit, func(raw json.RawMessage) (string, error) {
		var r wireRefund
		if err := decodeStrict(raw, &r); err != nil {
			return "", err
		}
		if strings.ToLower(r.Currency) == currency && r.Status == "succeeded" {
			bucket(r.Created).Refunded += r.Amount
		}
		return r.ID, nil
	})
	return current, previous, err
}

// Change is the upgrades and downgrades seen in the window: how many subscriptions and how much MRR moved.
type Change struct {
	Upgraded, Downgraded       int64
	UpgradedMRR, DowngradedMRR int64
}

type wireEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object             json.RawMessage `json:"object"`
		PreviousAttributes struct {
			Items *struct {
				Data []wireItem `json:"data"`
			} `json:"items"`
		} `json:"previous_attributes"`
	} `json:"data"`
}

// previous rebuilds the subscription as it was before an update event: the items the event lists as changed keep
// their old quantity and price, every other item and field stays as it is now.
func previous(cur Sub, curWire wireSub, prev []wireItem) Sub {
	old := cur
	old.Items = nil
	byID := map[string]wireItem{}
	for _, p := range prev {
		byID[p.ID] = p
	}
	for _, w := range curWire.Items.Data {
		if p, ok := byID[w.ID]; ok {
			if p.Quantity != nil {
				w.Quantity = p.Quantity
			}
			if p.Price != nil {
				w.Price = p.Price
			}
		}
		if item, ok := w.item(); ok {
			old.Items = append(old.Items, item)
		}
	}
	return old
}

// ChangesSince classifies subscription updates by whether they raised or lowered MRR. Stripe lists events for 30 days
// only, so the caller asks for a window it can cover.
func ChangesSince(ctx context.Context, g Getter, since, from, now int64) (current, prior Change, err error) {
	q := url.Values{"type": {"customer.subscription.updated"}, "created[gte]": {strconv.FormatInt(since, 10)}}
	_, err = walk(ctx, g, "/v1/events", q, maxEvents, func(raw json.RawMessage) (string, error) {
		var e wireEvent
		if err := decodeStrict(raw, &e); err != nil {
			return "", err
		}
		if e.Data.PreviousAttributes.Items == nil {
			return e.ID, nil
		}
		var w wireSub
		if err := decodeStrict(e.Data.Object, &w); err != nil {
			return "", err
		}
		cur := w.sub()
		if !counts(cur.Status) {
			return e.ID, nil
		}
		delta := cur.MRR(now) - previous(cur, w, e.Data.PreviousAttributes.Items.Data).MRR(now)
		c := &prior
		if e.Created >= from {
			c = &current
		}
		switch {
		case delta > 0:
			c.Upgraded++
			c.UpgradedMRR += delta
		case delta < 0:
			c.Downgraded++
			c.DowngradedMRR -= delta
		}
		return e.ID, nil
	})
	return current, prior, err
}

// Balance is the Stripe balance in the report currency and the next payout on its way.
type Balance struct {
	Available, Pending int64
	Payout             *Payout
}

// Payout is a payout that has not arrived yet.
type Payout struct {
	Amount  int64  `json:"amount"`
	Arrival int64  `json:"arrival"`
	Status  string `json:"status"`
}

// ReadBalance reads the balance and the earliest payout still pending or in transit.
func ReadBalance(ctx context.Context, g Getter, currency string) (Balance, error) {
	var bal struct {
		Available []struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"available"`
		Pending []struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"pending"`
	}
	if err := g.Get(ctx, "/v1/balance", &bal); err != nil {
		return Balance{}, err
	}
	var out Balance
	for _, a := range bal.Available {
		if strings.ToLower(a.Currency) == currency {
			out.Available += a.Amount
		}
	}
	for _, a := range bal.Pending {
		if strings.ToLower(a.Currency) == currency {
			out.Pending += a.Amount
		}
	}
	var payouts struct {
		Data []struct {
			Amount      int64  `json:"amount"`
			Currency    string `json:"currency"`
			Status      string `json:"status"`
			ArrivalDate int64  `json:"arrival_date"`
		} `json:"data"`
	}
	if err := g.Get(ctx, "/v1/payouts?limit=10", &payouts); err != nil {
		return Balance{}, err
	}
	for _, p := range payouts.Data {
		if (p.Status == "pending" || p.Status == "in_transit") && strings.ToLower(p.Currency) == currency && (out.Payout == nil || p.ArrivalDate < out.Payout.Arrival) {
			out.Payout = &Payout{Amount: p.Amount, Arrival: p.ArrivalDate, Status: p.Status}
		}
	}
	return out, nil
}

// Event is one line of Stripe's event log.
type Event struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	ObjectID string `json:"object_id,omitempty"`
}

// EventPage is one page of the log, newest first; Next is the cursor for the page after it.
type EventPage struct {
	Events []Event `json:"events"`
	Next   string  `json:"next_cursor,omitempty"`
}

// Events reads one page of Stripe's event log.
func Events(ctx context.Context, g Getter, cursor string, limit int) (EventPage, error) {
	limit = max(1, min(limit, 100))
	v := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		v.Set("starting_after", cursor)
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Created int64  `json:"created"`
			Data    struct {
				Object struct {
					ID string `json:"id"`
				} `json:"object"`
			} `json:"data"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	if err := g.Get(ctx, "/v1/events?"+v.Encode(), &out); err != nil {
		return EventPage{}, err
	}
	page := EventPage{Events: make([]Event, 0, len(out.Data))}
	for _, e := range out.Data {
		page.Events = append(page.Events, Event{ID: e.ID, Type: e.Type, Created: e.Created, ObjectID: e.Data.Object.ID})
	}
	if out.HasMore && len(page.Events) > 0 {
		page.Next = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

// failureCode names why a Stripe read failed, in the words the page translates.
func failureCode(err error) string {
	var api billing.StripeAPIError
	if errors.As(err, &api) {
		switch api.StatusCode {
		case 401, 403:
			return "unauthorized"
		case 429:
			return "rate_limited"
		case 400:
			return "rejected"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "provider_unavailable"
}
