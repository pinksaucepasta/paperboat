package billing

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// StripeTopupClient is separate from subscription and portal provider contracts.
type StripeTopupClient interface {
	CreateTopupCheckout(context.Context, StripeTopupInput) (CheckoutSession, error)
	GetTopupCheckout(context.Context, string) (StripeTopupState, error)
}
type StripeTopupInput struct {
	Quantity                                                                int
	CustomerID, PriceID, WorkspaceID, IdempotencyKey, SuccessURL, CancelURL string
}
type StripeTopupState struct {
	ID, CustomerID, WorkspaceID, Operation, Mode, Status, PaymentStatus, PriceID, URL string
	Quantity                                                                          int
	AmountTotal                                                                       int64
	Currency                                                                          string
	CreatedAt                                                                         time.Time
}

func (c HTTPStripeClient) CreateTopupCheckout(ctx context.Context, in StripeTopupInput) (CheckoutSession, error) {
	v := url.Values{
		"mode": {"payment"}, "customer": {in.CustomerID},
		"line_items[0][price]": {in.PriceID}, "line_items[0][quantity]": {strconv.Itoa(in.Quantity)},
		"success_url": {in.SuccessURL}, "cancel_url": {in.CancelURL},
		"metadata[paperboat_topup_operation]": {in.IdempotencyKey},
		"metadata[workspace_id]":              {in.WorkspaceID},
		"managed_payments[enabled]":           {"false"},
	}
	hash := sha256.Sum256([]byte(in.IdempotencyKey))
	suffix := make([]byte, 8)
	for i := range suffix {
		suffix[i] = 'a' + hash[i]%26
	}
	v.Set("integration_identifier", "paperboat_topup_"+string(suffix))
	var out struct{ ID, URL, Customer string }
	if err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions", in.IdempotencyKey, v, &out); err != nil {
		return CheckoutSession{}, err
	}
	if out.ID == "" || out.URL == "" || out.Customer != in.CustomerID {
		return CheckoutSession{}, errors.New("invalid stripe top-up checkout response")
	}
	return CheckoutSession{URL: out.URL, ProviderSessionID: out.ID, ProviderCustomerID: out.Customer}, nil
}
func (c HTTPStripeClient) GetTopupCheckout(ctx context.Context, id string) (StripeTopupState, error) {
	var out struct {
		ID, Customer, Mode, Status, URL string
		Currency                        string `json:"currency"`
		AmountTotal                     int64  `json:"amount_total"`
		PaymentStatus                   string `json:"payment_status"`
		Created                         int64  `json:"created"`
		Metadata                        struct {
			Operation string `json:"paperboat_topup_operation"`
			Workspace string `json:"workspace_id"`
		} `json:"metadata"`
		LineItems struct {
			HasMore bool `json:"has_more"`
			Data    []struct {
				Quantity int
				Price    struct{ ID string }
			}
		} `json:"line_items"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/checkout/sessions/"+url.PathEscape(id)+"?expand%5B%5D=line_items", "", nil, &out); err != nil {
		return StripeTopupState{}, err
	}
	if out.ID != id || out.Customer == "" || out.Created <= 0 {
		return StripeTopupState{}, errors.New("invalid stripe top-up checkout response")
	}
	s := StripeTopupState{AmountTotal: out.AmountTotal, Currency: out.Currency, ID: out.ID, CustomerID: out.Customer, Mode: out.Mode, Status: out.Status, URL: out.URL, PaymentStatus: out.PaymentStatus, Operation: out.Metadata.Operation, WorkspaceID: out.Metadata.Workspace, CreatedAt: time.Unix(out.Created, 0).UTC()}
	if len(out.LineItems.Data) == 1 && !out.LineItems.HasMore {
		s.PriceID = out.LineItems.Data[0].Price.ID
		s.Quantity = out.LineItems.Data[0].Quantity
	}
	return s, nil
}
