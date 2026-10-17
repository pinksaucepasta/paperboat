package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
)

const (
	StripeAPIVersion      = "2026-09-30.endive"
	stripeMaxResponseBody = 1 << 20
)

type StripeAPIError struct{ StatusCode int }

func (e StripeAPIError) Error() string {
	return fmt.Sprintf("stripe API returned status %d", e.StatusCode)
}

func (e StripeAPIError) DiagnosticStatus() int { return e.StatusCode }

type StripeClient interface {
	CreateCustomer(context.Context, string, string, string) (string, error)
	CreateSubscriptionCheckout(context.Context, StripeCheckoutInput) (CheckoutSession, error)
	GetCheckoutSession(context.Context, string) (StripeCheckoutState, error)
	CreatePortal(context.Context, string, string, string) (CustomerPortalSession, error)
	GetSubscription(context.Context, string) (StripeSubscription, error)
	UpdateSubscriptionSeats(context.Context, string, string, int, string) (StripeSubscription, error)
}

type StripeCheckoutState struct {
	ID, CustomerID, CheckoutOperation, Status, URL string
	Mode, SubscriptionID, PriceID                  string
	Quantity                                       int
}

type StripeCheckoutInput struct {
	CustomerID, PriceID                   string
	Quantity                              int
	SuccessURL, CancelURL, IdempotencyKey string
}

type StripeSubscription struct {
	ID, CustomerID, PriceID, ItemID, Status string
	CheckoutOperation                       string
	Quantity                                int
	PeriodStart, PeriodEnd                  time.Time
	CancelAtPeriodEnd                       bool
}

type HTTPStripeClient struct {
	BaseURL   string
	APIKey    string
	Client    *http.Client
	AllowLive bool
}

func (c HTTPStripeClient) CreateCustomer(ctx context.Context, workspaceID, email, key string) (string, error) {
	v := url.Values{"email": {email}, "metadata[workspace_id]": {workspaceID}}
	var out struct {
		ID       string `json:"id"`
		Livemode bool   `json:"livemode"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/customers", key, v, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("stripe customer response missing id")
	}
	return out.ID, nil
}

func (c HTTPStripeClient) CreateSubscriptionCheckout(ctx context.Context, in StripeCheckoutInput) (CheckoutSession, error) {
	v := url.Values{
		"mode": {"subscription"}, "customer": {in.CustomerID},
		"line_items[0][price]": {in.PriceID}, "line_items[0][quantity]": {strconv.Itoa(in.Quantity)},
		"success_url": {in.SuccessURL}, "cancel_url": {in.CancelURL},
		"metadata[paperboat_checkout_operation]":                    {in.IdempotencyKey},
		"subscription_data[metadata][paperboat_checkout_operation]": {in.IdempotencyKey},
	}
	// Keep the integration label stable across retries of the same operation.
	hash := sha256.Sum256([]byte(in.IdempotencyKey))
	suffix := make([]byte, 8)
	for i := range suffix {
		suffix[i] = 'a' + hash[i]%26
	}
	v.Set("integration_identifier", "paperboat_"+string(suffix))
	v.Set("managed_payments[enabled]", "false")
	var out struct {
		ID, URL, Customer string
		Livemode          bool `json:"livemode"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/checkout/sessions", in.IdempotencyKey, v, &out); err != nil {
		return CheckoutSession{}, err
	}
	if out.ID == "" || out.URL == "" || out.Customer != in.CustomerID {
		return CheckoutSession{}, errors.New("invalid stripe checkout response")
	}
	return CheckoutSession{URL: out.URL, ProviderSessionID: out.ID, ProviderCustomerID: out.Customer}, nil
}

func (c HTTPStripeClient) GetCheckoutSession(ctx context.Context, id string) (StripeCheckoutState, error) {
	var out struct {
		ID, Customer, Status, URL, Mode string
		Livemode                        bool            `json:"livemode"`
		Subscription                    json.RawMessage `json:"subscription"`
		LineItems                       struct {
			Data []struct {
				Quantity int `json:"quantity"`
				Price    struct {
					ID string `json:"id"`
				} `json:"price"`
			} `json:"data"`
		} `json:"line_items"`
		Metadata struct {
			Operation string `json:"paperboat_checkout_operation"`
		} `json:"metadata"`
	}
	path := "/v1/checkout/sessions/" + url.PathEscape(id) + "?expand%5B%5D=line_items"
	if err := c.do(ctx, http.MethodGet, path, "", nil, &out); err != nil {
		return StripeCheckoutState{}, err
	}
	if out.ID != id || out.Customer == "" || out.Status == "" {
		return StripeCheckoutState{}, errors.New("invalid stripe checkout response")
	}
	var subscriptionID string
	if len(out.Subscription) > 0 && string(out.Subscription) != "null" {
		if err := json.Unmarshal(out.Subscription, &subscriptionID); err != nil {
			var expanded struct {
				ID string `json:"id"`
			}
			if err = json.Unmarshal(out.Subscription, &expanded); err != nil {
				return StripeCheckoutState{}, errors.New("invalid stripe checkout response")
			}
			subscriptionID = expanded.ID
		}
	}
	state := StripeCheckoutState{ID: out.ID, CustomerID: out.Customer, CheckoutOperation: out.Metadata.Operation, Status: out.Status, URL: out.URL, Mode: out.Mode, SubscriptionID: subscriptionID}
	if len(out.LineItems.Data) == 1 {
		state.PriceID, state.Quantity = out.LineItems.Data[0].Price.ID, out.LineItems.Data[0].Quantity
	}
	return state, nil
}

func (c HTTPStripeClient) CreatePortal(ctx context.Context, customerID, returnURL, key string) (CustomerPortalSession, error) {
	var out struct {
		URL, Customer string
		Livemode      bool `json:"livemode"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/billing_portal/sessions", key, url.Values{"customer": {customerID}, "return_url": {returnURL}}, &out); err != nil {
		return CustomerPortalSession{}, err
	}
	if out.URL == "" || out.Customer != customerID {
		return CustomerPortalSession{}, errors.New("invalid stripe portal response")
	}
	return CustomerPortalSession{URL: out.URL}, nil
}

func (c HTTPStripeClient) GetSubscription(ctx context.Context, id string) (StripeSubscription, error) {
	var out stripeSubscriptionResponse
	if err := c.do(ctx, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(id), "", nil, &out); err != nil {
		return StripeSubscription{}, err
	}
	return out.subscription()
}

func (c HTTPStripeClient) UpdateSubscriptionSeats(ctx context.Context, subscriptionID, itemID string, quantity int, key string) (StripeSubscription, error) {
	v := url.Values{"items[0][id]": {itemID}, "items[0][quantity]": {strconv.Itoa(quantity)}}
	var out stripeSubscriptionResponse
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(subscriptionID), key, v, &out); err != nil {
		return StripeSubscription{}, err
	}
	s, err := out.subscription()
	if err != nil {
		return StripeSubscription{}, err
	}
	if s.ID != subscriptionID || s.ItemID != itemID || s.Quantity != quantity {
		return StripeSubscription{}, errors.New("stripe subscription update response does not match request")
	}
	return s, nil
}

type stripeSubscriptionResponse struct {
	ID, Customer, Status string
	Livemode             bool `json:"livemode"`
	CancelAtPeriodEnd    bool `json:"cancel_at_period_end"`
	Metadata             struct {
		PaperboatCheckoutOperation string `json:"paperboat_checkout_operation"`
	} `json:"metadata"`
	Items struct {
		Data []struct {
			ID                 string
			Quantity           int
			CurrentPeriodStart int64 `json:"current_period_start"`
			CurrentPeriodEnd   int64 `json:"current_period_end"`
			Price              struct{ ID string }
		}
	} `json:"items"`
}

func (r stripeSubscriptionResponse) subscription() (StripeSubscription, error) {
	if r.ID == "" || r.Customer == "" || r.Status == "" || len(r.Items.Data) != 1 {
		return StripeSubscription{}, errors.New("invalid stripe subscription response")
	}
	i := r.Items.Data[0]
	if i.ID == "" || i.Price.ID == "" || i.Quantity < 1 ||
		i.CurrentPeriodStart <= 0 || i.CurrentPeriodEnd <= i.CurrentPeriodStart {
		return StripeSubscription{}, errors.New("invalid stripe subscription response")
	}
	return StripeSubscription{
		ID: r.ID, CustomerID: r.Customer, PriceID: i.Price.ID, ItemID: i.ID,
		Status: r.Status, CheckoutOperation: r.Metadata.PaperboatCheckoutOperation,
		Quantity: i.Quantity, CancelAtPeriodEnd: r.CancelAtPeriodEnd,
		PeriodStart: time.Unix(i.CurrentPeriodStart, 0).UTC(),
		PeriodEnd:   time.Unix(i.CurrentPeriodEnd, 0).UTC(),
	}, nil
}

func (c HTTPStripeClient) do(ctx context.Context, method, path, key string, values url.Values, out any) error {
	if !c.AllowLive && (strings.HasPrefix(c.APIKey, "sk_live_") || strings.HasPrefix(c.APIKey, "rk_live_")) {
		return errors.New("live Stripe credentials are disabled")
	}
	client := c.Client
	if client == nil {
		client = serverdiagnostics.DefaultProviderClient("stripe")
	}
	base := c.BaseURL
	if base == "" {
		return ErrStripeUnavailable
	}
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Stripe-Version", StripeAPIVersion)
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, stripeMaxResponseBody))
		return StripeAPIError{StatusCode: resp.StatusCode}
	}
	limited := io.LimitReader(resp.Body, stripeMaxResponseBody+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(b) > stripeMaxResponseBody {
		return errors.New("stripe response exceeds size limit")
	}
	var live struct {
		Livemode bool `json:"livemode"`
	}
	if err := json.Unmarshal(b, &live); err != nil {
		return errors.New("invalid stripe response")
	}
	if live.Livemode && !c.AllowLive {
		return errors.New("stripe live-mode response rejected")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid stripe response")
	}
	return nil
}

type StripeWebhook struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID           string `json:"id"`
			Object       string `json:"object"`
			Subscription string `json:"subscription"`
			Parent       struct {
				Type                string `json:"type"`
				SubscriptionDetails struct {
					Subscription string `json:"subscription"`
				} `json:"subscription_details"`
			} `json:"parent"`
		} `json:"object"`
	} `json:"data"`
}

func ParseStripeWebhook(body []byte) (StripeWebhook, error) {
	var e StripeWebhook
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" || e.Type == "" {
		return StripeWebhook{}, errors.New("invalid stripe webhook")
	}
	return e, nil
}

func (e StripeWebhook) SubscriptionID() string {
	if e.Data.Object.Object == "subscription" {
		return e.Data.Object.ID
	}
	if e.Data.Object.Object == "invoice" {
		if e.Data.Object.Parent.Type == "subscription_details" {
			return e.Data.Object.Parent.SubscriptionDetails.Subscription
		}
		return ""
	}
	return e.Data.Object.Subscription
}

func VerifyStripeWebhookSignatureAt(body []byte, header string, secrets []string, tolerance time.Duration, now time.Time) error {
	var timestamp int64
	var signatures [][]byte
	for _, part := range strings.Split(header, ",") {
		p := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(p) != 2 {
			continue
		}
		switch p[0] {
		case "t":
			timestamp, _ = strconv.ParseInt(p[1], 10, 64)
		case "v1":
			if b, err := hex.DecodeString(p[1]); err == nil {
				signatures = append(signatures, b)
			}
		}
	}
	if timestamp == 0 || len(signatures) == 0 || len(secrets) == 0 {
		return errors.New("invalid stripe signature header")
	}
	age := now.Sub(time.Unix(timestamp, 0))
	if age < 0 {
		age = -age
	}
	if age > tolerance {
		return errors.New("stripe signature timestamp outside tolerance")
	}
	signed := append([]byte(strconv.FormatInt(timestamp, 10)+"."), body...)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(signed)
		expected := mac.Sum(nil)
		for _, got := range signatures {
			if hmac.Equal(expected, got) {
				return nil
			}
		}
	}
	return errors.New("invalid stripe signature")
}
