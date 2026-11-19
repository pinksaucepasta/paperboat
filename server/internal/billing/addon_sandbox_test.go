package billing

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// TestStripeSandboxAddon runs the add-on calls against a Stripe test-mode account: add, switch, seat purchase with the add-on
// item present, and remove, reading the subscription back through the same parser the webhook reconcile uses. It needs
// PB_STRIPE_SANDBOX_KEY (a test key; live keys are refused by the client) and PAPERBOAT_STRIPE_BASE_URL.
func TestStripeSandboxAddon(t *testing.T) {
	key, base := os.Getenv("PB_STRIPE_SANDBOX_KEY"), os.Getenv("PAPERBOAT_STRIPE_BASE_URL")
	if key == "" || base == "" {
		t.Skip("PB_STRIPE_SANDBOX_KEY and PAPERBOAT_STRIPE_BASE_URL are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := HTTPStripeClient{BaseURL: base, APIKey: key}
	x := strconv.FormatInt(time.Now().UnixNano(), 10)
	post := func(path string, v url.Values) (out struct{ ID string }) {
		t.Helper()
		if err := c.do(ctx, "POST", path, "", v, &out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return out
	}
	customer, err := c.CreateCustomer(ctx, "sandbox-addon-"+x, "sandbox-addon@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	post("/v1/payment_methods/pm_card_visa/attach", url.Values{"customer": {customer}})
	post("/v1/customers/"+customer, url.Values{"invoice_settings[default_payment_method]": {"pm_card_visa"}})
	product := post("/v1/products", url.Values{"name": {"Paperboat sandbox add-on test " + x}}).ID
	price := func(cents int64) string {
		return post("/v1/prices", url.Values{"product": {product}, "currency": {"usd"}, "unit_amount": {strconv.FormatInt(cents, 10)}, "recurring[interval]": {"month"}}).ID
	}
	b := config.Default().Business
	ssoCents, _ := b.AddonPriceCents("sso", "month")
	bothCents, _ := b.AddonPriceCents("sso_scim", "month")
	plan, sso, both := price(1500), price(ssoCents), price(bothCents)
	created := post("/v1/subscriptions", url.Values{"customer": {customer}, "items[0][price]": {plan}, "items[0][quantity]": {"2"}})
	defer func() {
		_ = c.do(context.Background(), "DELETE", "/v1/subscriptions/"+created.ID, "", nil, &struct{ ID string }{})
	}()

	get := func() StripeSubscription {
		t.Helper()
		s, err := c.GetSubscription(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base0 := get()
	if base0.PriceID != plan || base0.AddonItemID != "" || base0.Quantity != 2 {
		t.Fatalf("plain subscription = %+v", base0)
	}
	change := func(in AddonChange, k string) StripeSubscription {
		t.Helper()
		in.SubscriptionID, in.CustomerID, in.PlanItemID = created.ID, customer, base0.ItemID
		s, err := c.ChangeAddon(ctx, in, "addon-"+k+"-"+x)
		if err != nil {
			t.Fatalf("ChangeAddon %+v: %v", in, err)
		}
		return s
	}
	added := change(AddonChange{PriceID: sso, Addon: "sso"}, "add")
	if added.AddonPriceID != sso || added.AddonItemID == "" || added.PriceID != plan || added.ItemID != base0.ItemID {
		t.Fatalf("after add = %+v", added)
	}
	switched := change(AddonChange{AddonItemID: added.AddonItemID, PriceID: both, Addon: "sso_scim"}, "switch")
	if switched.AddonPriceID != both || switched.AddonItemID != added.AddonItemID {
		t.Fatalf("after switch = %+v", switched)
	}
	// A seat purchase keeps working with the add-on item on the subscription.
	seats, err := c.UpdateSubscriptionSeats(ctx, created.ID, base0.ItemID, 3, "addon-seats-"+x)
	if err != nil || seats.Quantity != 3 || seats.AddonPriceID != both {
		t.Fatalf("seat purchase = %+v, %v", seats, err)
	}
	removed := change(AddonChange{AddonItemID: switched.AddonItemID}, "remove")
	if removed.AddonItemID != "" || removed.PriceID != plan || get().AddonItemID != "" {
		t.Fatalf("after remove = %+v", removed)
	}
}
