package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStripeRejectsLiveKeyBeforeSendingRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("live credential reached provider") }))
	defer server.Close()
	for _, key := range []string{"sk_live_test", "rk_live_test"} {
		client := HTTPStripeClient{BaseURL: server.URL, APIKey: key, Client: server.Client()}
		if _, err := client.CreateCustomer(context.Background(), "team:one", "test@example.test", "op"); err == nil {
			t.Fatal("live key accepted")
		}
	}
}

func TestHTTPStripeClientRequestsPreserveWorkspaceCustomerAndQuantity(t *testing.T) {
	var requests []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Stripe-Version") != StripeAPIVersion || r.Header.Get("Authorization") != "Bearer sk_test_secret" {
			t.Errorf("headers = %#v", r.Header)
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			requests = append(requests, r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/customers":
			io.WriteString(w, `{"id":"cus_team","livemode":false}`)
		case "/v1/checkout/sessions":
			io.WriteString(w, `{"id":"cs_test_1","url":"https://checkout.stripe.test/cs_test_1","customer":"cus_team","livemode":false}`)
		case "/v1/subscriptions/sub_team":
			io.WriteString(w, `{"id":"sub_team","customer":"cus_team","status":"active","livemode":false,"metadata":{"paperboat_checkout_operation":"checkout-op"},"current_period_start":1700000000,"current_period_end":1702592000,"cancel_at_period_end":false,"items":{"data":[{"id":"si_team","quantity":4,"price":{"id":"price_team"}}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := HTTPStripeClient{BaseURL: srv.URL, APIKey: "sk_test_secret", Client: srv.Client()}
	id, err := c.CreateCustomer(context.Background(), "team_1", "owner@example.com", "customer-op")
	if err != nil || id != "cus_team" {
		t.Fatalf("customer=%q err=%v", id, err)
	}
	s, err := c.CreateSubscriptionCheckout(context.Background(), StripeCheckoutInput{CustomerID: id, PriceID: "price_team", Quantity: 4, SuccessURL: "https://paperboat.test/success", CancelURL: "https://paperboat.test/cancel", IdempotencyKey: "checkout-op"})
	if err != nil || s.ProviderCustomerID != id {
		t.Fatalf("checkout=%+v err=%v", s, err)
	}
	sub, err := c.UpdateSubscriptionSeats(context.Background(), "sub_team", "si_team", 4, "seats-op")
	if err != nil || sub.Quantity != 4 || sub.CheckoutOperation != "checkout-op" {
		t.Fatalf("subscription=%+v err=%v", sub, err)
	}
	if requests[0].Get("metadata[workspace_id]") != "team_1" || requests[1].Get("customer") != "cus_team" || requests[1].Get("line_items[0][quantity]") != "4" || requests[1].Get("metadata[paperboat_checkout_operation]") != "checkout-op" || requests[1].Get("subscription_data[metadata][paperboat_checkout_operation]") != "checkout-op" || requests[2].Get("items[0][id]") != "si_team" || requests[2].Get("items[0][quantity]") != "4" {
		t.Fatalf("forms=%#v", requests)
	}
}

func TestHTTPStripeClientGetPortalErrorsAndLiveGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/billing_portal/sessions":
			r.ParseForm()
			if r.PostForm.Get("customer") != "cus_only" {
				t.Error("customer isolation lost")
			}
			io.WriteString(w, `{"url":"https://billing.stripe.test/test","customer":"cus_only","livemode":false}`)
		case "/v1/subscriptions/sub_live":
			io.WriteString(w, `{"livemode":true}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, strings.Repeat("secret-response", 100000))
		}
	}))
	defer srv.Close()
	c := HTTPStripeClient{BaseURL: srv.URL, APIKey: "sk_test_hidden", Client: srv.Client()}
	portal, err := c.CreatePortal(context.Background(), "cus_only", "https://paperboat.test/account", "portal-op")
	if err != nil || portal.URL == "" {
		t.Fatalf("portal=%+v err=%v", portal, err)
	}
	_, err = c.GetSubscription(context.Background(), "sub_live")
	if err == nil || strings.Contains(err.Error(), "sk_test") {
		t.Fatalf("live error=%v", err)
	}
	_, err = c.GetSubscription(context.Background(), "missing")
	var apiErr StripeAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || strings.Contains(err.Error(), "secret-response") {
		t.Fatalf("api error=%v", err)
	}
}

func TestHTTPStripeClientRetrievesExpandedCheckoutIdentity(t *testing.T) {
	malformed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("expand[]") != "line_items" {
			t.Errorf("expand=%q", r.URL.RawQuery)
		}
		if malformed {
			io.WriteString(w, `{"id":"wrong","customer":"cus_one","status":"complete","livemode":false}`)
			return
		}
		io.WriteString(w, `{"id":"cs_one","customer":"cus_one","status":"complete","mode":"subscription","url":"https://checkout.stripe.test/cs_one","subscription":{"id":"sub_one"},"metadata":{"paperboat_checkout_operation":"checkout-one"},"line_items":{"data":[{"quantity":3,"price":{"id":"price_one"}}]},"livemode":false}`)
	}))
	defer srv.Close()
	c := HTTPStripeClient{BaseURL: srv.URL, APIKey: "sk_test_hidden", Client: srv.Client()}
	got, err := c.GetCheckoutSession(context.Background(), "cs_one")
	if err != nil || got.CustomerID != "cus_one" || got.Mode != "subscription" || got.SubscriptionID != "sub_one" || got.CheckoutOperation != "checkout-one" || got.PriceID != "price_one" || got.Quantity != 3 {
		t.Fatalf("checkout=%+v err=%v", got, err)
	}
	malformed = true
	if _, err = c.GetCheckoutSession(context.Background(), "cs_one"); err == nil {
		t.Fatal("mismatched session id accepted")
	}
}

func TestStripeWebhookSignatureRotationToleranceAndParsing(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := []byte(`{"id":"evt_1","type":"customer.subscription.updated","data":{"object":{"id":"sub_1","object":"subscription"}}}`)
	mac := hmac.New(sha256.New, []byte("whsec_new"))
	mac.Write(append([]byte(strconv.FormatInt(now.Unix(), 10)+"."), body...))
	header := "t=" + strconv.FormatInt(now.Unix(), 10) + ",v1=00,v1=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifyStripeWebhookSignatureAt(body, header, []string{"whsec_old", "whsec_new"}, 5*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStripeWebhookSignatureAt(body, header, []string{"wrong"}, 5*time.Minute, now); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if err := VerifyStripeWebhookSignatureAt(body, header, []string{"whsec_new"}, time.Minute, now.Add(2*time.Minute)); err == nil {
		t.Fatal("stale signature accepted")
	}
	e, err := ParseStripeWebhook(body)
	if err != nil || e.SubscriptionID() != "sub_1" {
		t.Fatalf("event=%+v err=%v", e, err)
	}
}

func TestStripeOutcomeUncertainClassification(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if !stripeOutcomeUncertain(StripeAPIError{StatusCode: status}) {
			t.Fatalf("status %d treated definitive", status)
		}
	}
	if stripeOutcomeUncertain(StripeAPIError{StatusCode: http.StatusBadRequest}) {
		t.Fatal("validation failure treated uncertain")
	}
}
