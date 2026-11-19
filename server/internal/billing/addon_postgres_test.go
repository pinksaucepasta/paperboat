package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// addonStripeFake is a Stripe whose subscription keeps one add-on item, like the real one.
type addonStripeFake struct {
	*membershipStripeFake
	calls []billing.AddonChange
}

func (f *addonStripeFake) ChangeAddon(_ context.Context, in billing.AddonChange, _ string) (billing.StripeSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	sub := f.subs[in.SubscriptionID]
	switch {
	case in.PriceID == "":
		sub.AddonItemID, sub.AddonPriceID = "", ""
	case in.AddonItemID == "":
		sub.AddonItemID, sub.AddonPriceID = "si_addon", in.PriceID
	default:
		sub.AddonPriceID = in.PriceID
	}
	f.subs[in.SubscriptionID] = sub
	return sub, nil
}

func TestTeamAddonBuySwitchRemovePostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, team := "usr_addon_"+x, "addon-"+x
	insertUser(t, store, owner, owner+"@example.test")
	dropAddonProducts(t, store)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	plan, planPrice := "addon-plan-"+x, "price_plan_"+x
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,$1,'stripe',$1,$2,'team_subscription','{}',true,1500,'usd','month')`, plan, planPrice)
	price := map[string]string{}
	for _, a := range billing.Addons {
		price[a] = "price_" + a + "_" + x
		// The add-on product codes are global: only this test's prices are read through its own price ids.
		exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,$2,'stripe',$1,$3,'team_addon','{}',true,10000,'usd','month')`, "p_"+a+x, billing.AddonProductCode(a, "month"), price[a])
	}
	start, end := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(24*time.Hour).UTC().Truncate(time.Second)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,provider_subscription_id,provider_item_id,provider_customer_id,product_code,period_start,period_end) VALUES('team:'||$1,$1,'active',2,'sub_'||$1,'si_'||$1,'cus_'||$1,$2,$3,$4)`, team, plan, start, end)
	sub := billing.StripeSubscription{ID: "sub_" + team, CustomerID: "cus_" + team, ItemID: "si_" + team, PriceID: planPrice, Status: "active", Quantity: 2, PeriodStart: start, PeriodEnd: end}
	fake := &addonStripeFake{membershipStripeFake: &membershipStripeFake{subs: map[string]billing.StripeSubscription{sub.ID: sub}, fail: map[string]bool{}, updates: map[string]int{}}}
	s := billing.NewWorkspaceService(store, fake, "addon-test-key")

	if _, err := s.ChangeAddon(ctx, owner, team, "sso", false, "k1"+x); !errors.Is(err, billing.ErrAddonConsent) {
		t.Fatalf("without consent: %v", err)
	}
	if _, err := s.ChangeAddon(ctx, owner, team, "saml", true, "k2"+x); !errors.Is(err, billing.ErrAddonInvalid) {
		t.Fatalf("unknown add-on: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatal("a refused request reached Stripe")
	}
	w, err := s.ChangeAddon(ctx, owner, team, "sso", true, "k3"+x)
	if err != nil || w.Addon != "sso" || !w.SSOAddon || w.SCIMAddon || len(fake.calls) != 1 || fake.calls[0].PriceID != price["sso"] || fake.calls[0].AddonItemID != "" {
		t.Fatalf("buy sso: %+v err=%v calls=%+v", w, err, fake.calls)
	}
	// The same add-on again changes nothing.
	if _, err = s.ChangeAddon(ctx, owner, team, "sso", true, "k4"+x); err != nil || len(fake.calls) != 1 {
		t.Fatalf("repeat: err=%v calls=%d", err, len(fake.calls))
	}
	w, err = s.ChangeAddon(ctx, owner, team, "sso_scim", true, "k5"+x)
	if err != nil || w.Addon != "sso_scim" || !w.SSOAddon || !w.SCIMAddon || fake.calls[1].AddonItemID != "si_addon" || fake.calls[1].PriceID != price["sso_scim"] {
		t.Fatalf("switch: %+v err=%v calls=%+v", w, err, fake.calls)
	}
	// A seat purchase keeps the add-on item (the subscription has two items).
	if got := fake.subs[sub.ID].AddonItemID; got != "si_addon" {
		t.Fatalf("add-on item lost: %q", got)
	}
	w, err = s.ChangeAddon(ctx, owner, team, "", false, "k6"+x)
	if err != nil || w.AddonUntil == nil || !w.AddonUntil.Equal(end) || !w.SSOAddon || !w.SCIMAddon || fake.calls[2].PriceID != "" {
		t.Fatalf("remove keeps the paid period: %+v err=%v", w, err)
	}
	exec(`UPDATE paperboat.billing_workspaces SET addon_until=now()-interval '1 minute' WHERE team_id=$1`, team)
	if w, err = s.Overview(ctx, owner, team); err != nil || w.SSOAddon || w.SCIMAddon {
		t.Fatalf("after the paid period: %+v err=%v", w, err)
	}
	// Buying again after a removal clears the end date.
	if w, err = s.ChangeAddon(ctx, owner, team, "scim", true, "k7"+x); err != nil || w.AddonUntil != nil || !w.SCIMAddon || w.SSOAddon {
		t.Fatalf("buy after removal: %+v err=%v", w, err)
	}
	exec(`UPDATE paperboat.billing_workspaces SET cancel_at_period_end=true WHERE team_id=$1`, team)
	if _, err = s.ChangeAddon(ctx, owner, team, "sso", true, "k8"+x); !errors.Is(err, billing.ErrAddonSubscription) {
		t.Fatalf("cancelling plan: %v", err)
	}
	exec(`UPDATE paperboat.billing_workspaces SET cancel_at_period_end=false WHERE team_id=$1`, team)
	exec(`UPDATE paperboat.billing_products SET active=false WHERE provider_price_id=$1`, price["sso"])
	if _, err = s.ChangeAddon(ctx, owner, team, "sso", true, "k9"+x); !errors.Is(err, billing.ErrAddonUnavailable) {
		t.Fatalf("add-on without a price: %v", err)
	}
	exec(`UPDATE paperboat.billing_products SET active=true WHERE provider_price_id=$1`, price["sso"])
}

func TestAddonPriceMustMatchTheConfiguredPricePostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "usr_addon_op_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,role,status) VALUES($1,$1||'@example.test','unused test login','admin','active')`, admin); err != nil {
		t.Fatal(err)
	}
	dropAddonProducts(t, store)
	put := func(code, id string, amount int64, interval string) error {
		p := billing.StripePrice{ID: id, ProductID: "prod_" + id, Currency: "usd", Interval: interval, UnitAmount: amount, Active: true, Recurring: true, Licensed: true}
		_, err := billing.NewOperatorService(store, operatorPriceReader{p}).PutProduct(ctx, admin, "key_"+id, code, "test", billing.ProductInput{DisplayName: "SSO", ProviderPriceID: id, CatalogType: billing.AddonCatalog, Active: true})
		return err
	}
	code := billing.AddonProductCode("sso", "month")
	if err := put(code, "price_wrong_"+x, 9900, "month"); !errors.Is(err, billing.ErrOperatorBillingInvalid) {
		t.Fatalf("wrong amount accepted: %v", err)
	}
	if err := put("addon-other-month", "price_code_"+x, 10000, "month"); !errors.Is(err, billing.ErrOperatorBillingInvalid) {
		t.Fatalf("unknown code accepted: %v", err)
	}
	if err := put(billing.AddonProductCode("sso_scim", "year"), "price_year_"+x, 150000, "year"); err != nil {
		t.Fatalf("configured yearly price refused: %v", err)
	}
}

// dropAddonProducts clears the add-on products (their codes are global) before and after a test.
func dropAddonProducts(t *testing.T, store *db.DB) {
	t.Helper()
	clear := func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE code LIKE 'addon-%'`)
	}
	clear()
	t.Cleanup(clear)
}
