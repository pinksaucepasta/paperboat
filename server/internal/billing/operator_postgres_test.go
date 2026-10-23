package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

type operatorPriceReader struct{ price billing.StripePrice }

func (r operatorPriceReader) GetPrice(context.Context, string) (billing.StripePrice, error) {
	return r.price, nil
}

func TestOperatorBillingCatalogAndPolicyPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin, billingUser, ordinary := "operator_admin_"+x, "operator_billing_"+x, "usr_ordinary_"+x
	insertUser(t, store, ordinary, ordinary+"@example.test")
	for _, actor := range []struct{ id, role string }{{admin, "admin"}, {billingUser, "billing"}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,role,status) VALUES($1,$1||'@example.test','unused test login',$2,'active')`, actor.id, actor.role); err != nil {
			t.Fatal(err)
		}
	}
	code, priceID, productID := "plan_"+x, "price_"+x, "prod_"+x
	legacyCode := "legacy_" + x
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspace_operations WHERE actor_user_id IN ($1,$2,$3)`, admin, billingUser, ordinary)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, "personal:"+ordinary)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspaces WHERE user_id=$1`, ordinary)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_operator_operations WHERE actor_user_id IN ($1,$2,$3)`, admin, billingUser, ordinary)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.audit_events WHERE actor_user_id IN ($1,$2,$3)`, admin, billingUser, ordinary)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_products WHERE code=$1`, code)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_products WHERE code=$1`, legacyCode)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, ordinary)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.operator_accounts WHERE id IN ($1,$2)`, admin, billingUser)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, ordinary)
	})
	stripeCheckout := &workspaceStripeFake{}
	workspace := billing.NewWorkspaceService(store, stripeCheckout, "operator-catalog-test-key")
	if err := workspace.InitializeAllowancePolicy(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	prices := operatorPriceReader{billing.StripePrice{ID: priceID, ProductID: productID, Currency: "usd", Interval: "month", UnitAmount: 2500, Active: true, Recurring: true, Licensed: true}}
	service := billing.NewOperatorService(store, prices)
	originalPolicy, err := service.Policy(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=$1,team_allowance_bytes=$2,revision=$3,updated_at=now() WHERE singleton`, originalPolicy.PersonalAllowanceBytes, originalPolicy.TeamAllowanceBytes, originalPolicy.Revision)
	})
	allowance := int64(4096)
	input := billing.ProductInput{DisplayName: "Operator Plan", ProviderPriceID: priceID, CatalogType: "personal_subscription", Active: true, AllowanceBytes: &allowance}
	badProvider := billing.NewOperatorService(store, operatorPriceReader{billing.StripePrice{ID: "price_other", ProductID: productID, Currency: "usd", Interval: "month", UnitAmount: 2500, Active: true, Recurring: true, Licensed: true}})
	if _, err := badProvider.PutProduct(ctx, admin, "mismatch-"+x, code, "provider mismatch", input); !errors.Is(err, billing.ErrOperatorBillingInvalid) {
		t.Fatalf("provider mismatch err=%v", err)
	}
	inactiveProvider := billing.NewOperatorService(store, operatorPriceReader{billing.StripePrice{ID: priceID, ProductID: productID, Currency: "usd", Interval: "month", UnitAmount: 2500, Recurring: true, Licensed: true}})
	if _, err := inactiveProvider.PutProduct(ctx, admin, "inactive-"+x, code, "inactive price", input); !errors.Is(err, billing.ErrOperatorBillingInvalid) {
		t.Fatalf("inactive price err=%v", err)
	}
	if _, err := service.PutProduct(ctx, billingUser, "billing-forbidden-"+x, code, "test", input); !errors.Is(err, billing.ErrOperatorBillingForbidden) {
		t.Fatalf("billing mutation err=%v", err)
	}
	created, err := service.PutProduct(ctx, admin, "create-"+x, code, "launch verified price", input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.PutProduct(ctx, admin, "create-"+x, code, "launch verified price", input)
	if err != nil || replayed.Version != created.Version || replayed.Code != created.Code || replayed.AllowanceBytes == nil || *replayed.AllowanceBytes != allowance {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	changed := input
	changed.DisplayName = "Changed"
	if _, err = service.PutProduct(ctx, admin, "create-"+x, code, "launch verified price", changed); !errors.Is(err, billing.ErrOperatorBillingConflict) {
		t.Fatalf("idempotency conflict=%v", err)
	}
	if _, err = service.PutProduct(ctx, admin, "stale-"+x, code, "stale", billing.ProductInput{Version: created.Version + 1, DisplayName: "Stale", ProviderPriceID: priceID, CatalogType: "personal_subscription", Active: true}); !errors.Is(err, billing.ErrOperatorBillingConflict) {
		t.Fatalf("CAS err=%v", err)
	}
	wrongType := input
	wrongType.Version = created.Version
	wrongType.CatalogType = "team_subscription"
	if _, err = service.PutProduct(ctx, admin, "type-change-"+x, code, "wrong scope", wrongType); !errors.Is(err, billing.ErrOperatorBillingConflict) {
		t.Fatalf("catalog type rebind err=%v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$2,'stripe',$3,$4,'personal_subscription','{}',false)`, "bp_legacy_"+x, legacyCode, "prod_legacy_"+x, "price_legacy_"+x); err != nil {
		t.Fatal(err)
	}
	page, err := service.ListProducts(ctx, billingUser, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range page.Products {
		if p.Code == code {
			found = p.PriceVerified && p.ProviderPriceID == priceID && p.ProviderProductID == productID && p.UnitAmount == 2500 && p.AllowanceBytes != nil && *p.AllowanceBytes == allowance
		}
		if p.Code == legacyCode && p.PriceVerified {
			t.Fatal("legacy unverified zero price presented as verified")
		}
	}
	if !found {
		t.Fatalf("product absent/unsafe: %+v", page)
	}
	products, err := workspace.Products(ctx, ordinary, "", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, p := range products.Items {
		seen = seen || p.Code == code
	}
	if !seen {
		t.Fatal("operator product not consumed by checkout catalog")
	}
	if _, err = workspace.Checkout(ctx, ordinary, ordinary+"@example.test", "", code, "checkout-"+x, "https://example.test/success", "https://example.test/cancel", 1); err != nil {
		t.Fatal(err)
	}
	if len(stripeCheckout.checkoutInputs) != 1 || stripeCheckout.checkoutInputs[0].PriceID != priceID {
		t.Fatalf("checkout did not consume configured price: %+v", stripeCheckout.checkoutInputs)
	}
	deactivatedInput := input
	deactivatedInput.Version = created.Version
	deactivatedInput.Active = false
	deactivated, err := inactiveProvider.PutProduct(ctx, admin, "deactivate-"+x, code, "retire checkout", deactivatedInput)
	if err != nil || deactivated.Active {
		t.Fatalf("deactivate=%+v err=%v", deactivated, err)
	}
	products, err = workspace.Products(ctx, ordinary, "", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range products.Items {
		if p.Code == code {
			t.Fatal("inactive product remained in new checkout catalog")
		}
	}
	if _, err = workspace.Checkout(ctx, ordinary, ordinary+"@example.test", "", code, "checkout-inactive-"+x, "https://example.test/success", "https://example.test/cancel", 1); !errors.Is(err, billing.ErrUnknownProduct) {
		t.Fatalf("inactive checkout err=%v", err)
	}
	if _, err = service.ListProducts(ctx, ordinary, "", 10); !errors.Is(err, billing.ErrOperatorBillingForbidden) {
		t.Fatalf("ordinary catalog read=%v", err)
	}

	policy, err := service.Policy(ctx, billingUser)
	if err != nil {
		t.Fatal(err)
	}
	personal := int64(0)
	policy.PersonalAllowanceBytes = &personal
	updated, err := service.PutPolicy(ctx, admin, "policy-"+x, "set free limits", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PutPolicy(ctx, admin, "policy-stale-"+x, "stale", policy); !errors.Is(err, billing.ErrOperatorBillingConflict) {
		t.Fatalf("policy CAS=%v", err)
	}
	if updated.Revision != policy.Revision+1 {
		t.Fatalf("revision=%d", updated.Revision)
	}

	trigger := "reject_billing_audit_" + x
	fn := "reject_billing_audit_fn_" + x
	if _, err = store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION paperboat.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='billing.operator.put_policy' THEN RAISE EXCEPTION 'audit failure'; END IF; RETURN NEW; END $$`, fn)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON paperboat.audit_events FOR EACH ROW EXECUTE FUNCTION paperboat.%s()`, trigger, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON paperboat.audit_events`, trigger))
		_, _ = store.SQL().ExecContext(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS paperboat.%s()`, fn))
	})
	before, err := service.Policy(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	attempt := before
	team := int64(9)
	attempt.TeamAllowanceBytes = &team
	if _, err = service.PutPolicy(ctx, admin, "audit-fail-"+x, "must rollback", attempt); err == nil {
		t.Fatal("audit failure committed policy")
	}
	after, err := service.Policy(ctx, admin)
	if err != nil || after.Revision != before.Revision || after.TeamAllowanceBytes != before.TeamAllowanceBytes {
		t.Fatalf("policy changed across audit failure: before=%+v after=%+v err=%v", before, after, err)
	}
}
