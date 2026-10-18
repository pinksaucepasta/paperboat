package billing

import (
	"context"
	"net/http"
	"net/url"
)

// PaidTeamCheckout is separate from generic checkout recovery: only an actual
// paid Checkout session may authorize initial Team materialization.
func (c HTTPStripeClient) PaidTeamCheckout(ctx context.Context, id string) (StripeCheckoutState, string, error) {
	var out struct {
		ID, Customer, Status, URL, Mode, Subscription string
		PaymentStatus                                 string `json:"payment_status"`
		Metadata                                      struct {
			Operation string `json:"paperboat_checkout_operation"`
		} `json:"metadata"`
		LineItems struct {
			Data []struct {
				Quantity int
				Price    struct{ ID string }
			}
			HasMore bool `json:"has_more"`
		} `json:"line_items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/checkout/sessions/"+url.PathEscape(id)+"?expand%5B%5D=line_items", "", nil, &out)
	if err != nil {
		return StripeCheckoutState{}, "", err
	}
	state := StripeCheckoutState{ID: out.ID, CustomerID: out.Customer, Status: out.Status, URL: out.URL, Mode: out.Mode, SubscriptionID: out.Subscription, CheckoutOperation: out.Metadata.Operation}
	if len(out.LineItems.Data) == 1 && !out.LineItems.HasMore {
		state.PriceID = out.LineItems.Data[0].Price.ID
		state.Quantity = out.LineItems.Data[0].Quantity
	}
	return state, out.PaymentStatus, nil
}
