package billing

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

type seatInvoice struct {
	AmountDue int64  `json:"amount_due"`
	Currency  string `json:"currency"`
	Status    string `json:"status"`
}

func (c HTTPStripeClient) PreviewSeats(ctx context.Context, in SeatChange) (SeatPreview, error) {
	values := url.Values{"subscription": {in.SubscriptionID}, "subscription_details[items][0][id]": {in.ItemID}, "subscription_details[items][0][quantity]": {strconv.Itoa(in.Quantity)}, "subscription_details[proration_behavior]": {"none"}}
	var next seatInvoice
	if err := c.do(ctx, http.MethodPost, "/v1/invoices/create_preview", "", values, &next); err != nil {
		return SeatPreview{}, err
	}
	if !stripeCurrencyPattern.MatchString(next.Currency) {
		return SeatPreview{}, errors.New("stripe seat preview has invalid currency")
	}
	out := SeatPreview{Currency: next.Currency, NextInvoiceAmount: next.AmountDue, NextInvoiceAt: in.PeriodEnd}
	if in.Quantity > in.PreviousQuantity {
		values.Set("subscription_details[proration_behavior]", "always_invoice")
		values.Set("subscription_details[proration_date]", strconv.FormatInt(in.ProrationAt, 10))
		var immediate seatInvoice
		if err := c.do(ctx, http.MethodPost, "/v1/invoices/create_preview", "", values, &immediate); err != nil {
			return SeatPreview{}, err
		}
		if immediate.Currency != out.Currency {
			return SeatPreview{}, errors.New("stripe preview currency changed")
		}
		out.AmountDueNow = immediate.AmountDue
	}
	return out, nil
}

func (c HTTPStripeClient) seatSchedule(ctx context.Context, in SeatChange) (string, error) {
	var current struct {
		ID, Customer string
		Schedule     *struct{ ID string } `json:"schedule"`
		Items        struct {
			Data []struct {
				ID       string
				Quantity int
				Price    struct{ ID string }
				Start    int64 `json:"current_period_start"`
				End      int64 `json:"current_period_end"`
			} `json:"data"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(in.SubscriptionID)+"?expand%5B%5D=schedule", "", nil, &current); err != nil {
		return "", err
	}
	if current.ID != in.SubscriptionID || current.Customer != in.CustomerID {
		return "", ErrProviderOutcomeUnknown
	}
	if len(current.Items.Data) != 1 {
		return "", ErrProviderOutcomeUnknown
	}
	item := current.Items.Data[0]
	if item.ID != in.ItemID || item.Price.ID != in.PriceID || item.Start != in.PeriodStart.Unix() || item.End != in.PeriodEnd.Unix() || (item.Quantity != in.PreviousQuantity && item.Quantity != in.Quantity) {
		return "", ErrProviderOutcomeUnknown
	}
	if current.Schedule != nil {
		return current.Schedule.ID, nil
	}
	return "", nil
}

func (c HTTPStripeClient) ChangeSeats(ctx context.Context, in SeatChange, key string) (SeatPreview, error) {
	schedule, err := c.seatSchedule(ctx, in)
	if err != nil {
		return SeatPreview{}, err
	}
	out := in.Preview
	if in.Quantity < in.PreviousQuantity {
		if schedule == "" {
			var created struct{ ID string }
			if err = c.do(ctx, http.MethodPost, "/v1/subscription_schedules", key+":schedule", url.Values{"from_subscription": {in.SubscriptionID}}, &created); err != nil {
				return SeatPreview{}, err
			}
			schedule = created.ID
		}
		if schedule == "" {
			return SeatPreview{}, ErrProviderOutcomeUnknown
		}
		price, err := c.GetPrice(ctx, in.PriceID)
		if err != nil {
			return SeatPreview{}, errors.Join(ErrProviderOutcomeUnknown, err)
		}
		values := url.Values{
			"end_behavior": {"release"}, "proration_behavior": {"none"},
			"phases[0][start_date]":      {strconv.FormatInt(in.PeriodStart.Unix(), 10)},
			"phases[0][end_date]":        {strconv.FormatInt(in.PeriodEnd.Unix(), 10)},
			"phases[0][items][0][price]": {in.PriceID}, "phases[0][items][0][quantity]": {strconv.Itoa(in.PreviousQuantity)},
			"phases[0][proration_behavior]": {"none"},
			"phases[1][start_date]":         {strconv.FormatInt(in.PeriodEnd.Unix(), 10)},
			"phases[1][items][0][price]":    {in.PriceID}, "phases[1][items][0][quantity]": {strconv.Itoa(in.Quantity)},
			"phases[1][proration_behavior]": {"none"},
			"phases[1][duration][interval]": {price.Interval}, "phases[1][duration][interval_count]": {"1"},
		}
		var updated struct{ ID string }
		if err = c.do(ctx, http.MethodPost, "/v1/subscription_schedules/"+url.PathEscape(schedule), key+":phases", values, &updated); err != nil {
			return SeatPreview{}, err
		}
		if updated.ID != schedule {
			return SeatPreview{}, ErrProviderOutcomeUnknown
		}
		out.AmountDueNow = 0
		return out, nil
	}
	if schedule != "" {
		var released struct{ ID string }
		if err = c.do(ctx, http.MethodPost, "/v1/subscription_schedules/"+url.PathEscape(schedule)+"/release", key+":release", url.Values{}, &released); err != nil {
			return SeatPreview{}, errors.Join(ErrProviderOutcomeUnknown, err)
		}
		if released.ID != schedule {
			return SeatPreview{}, ErrProviderOutcomeUnknown
		}
	}
	if in.Quantity == in.PreviousQuantity {
		out.AmountDueNow = 0
		return out, nil
	}
	values := url.Values{"items[0][id]": {in.ItemID}, "items[0][quantity]": {strconv.Itoa(in.Quantity)},
		"proration_behavior": {"always_invoice"}, "payment_behavior": {"error_if_incomplete"},
		"proration_date": {strconv.FormatInt(in.ProrationAt, 10)}, "expand[]": {"latest_invoice"}}
	var response struct {
		stripeSubscriptionResponse
		Invoice *seatInvoice `json:"latest_invoice"`
	}
	if err = c.do(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(in.SubscriptionID), key, values, &response); err != nil {
		if schedule != "" || in.HadScheduledReduction {
			return SeatPreview{}, errors.Join(ErrProviderOutcomeUnknown, err)
		}
		return SeatPreview{}, err
	}
	// Decode invoice separately: the subscription parser uses only renewal metadata.
	sub, err := response.subscription()
	if err != nil {
		return SeatPreview{}, err
	}
	if sub.ID != in.SubscriptionID || sub.CustomerID != in.CustomerID || sub.ItemID != in.ItemID || sub.Quantity != in.Quantity || !sub.PeriodEnd.Equal(in.PeriodEnd) || !sub.PeriodStart.Equal(in.PeriodStart) || response.Invoice == nil || response.Invoice.Status != "paid" {
		return SeatPreview{}, ErrProviderOutcomeUnknown
	}
	out.AmountDueNow = response.Invoice.AmountDue
	out.Currency = response.Invoice.Currency
	return out, nil
}
