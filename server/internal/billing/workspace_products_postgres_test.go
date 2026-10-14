package billing_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

func TestWorkspaceProductPagesAndPriceReviewFence(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	account := "usr_prices_" + suffix
	prefix := "plans_" + suffix + "_"
	insertUser(t, store, account, account+"@example.test")
	_, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) SELECT $1||lpad(n::text,3,'0'),$1||lpad(n::text,3,'0'),'stripe','prod_'||$1||n,'price_'||$1||n,'personal_subscription','{"display_name":"Test subscription"}',true,CASE WHEN n=202 THEN NULL ELSE 2500 END,CASE WHEN n=202 THEN NULL ELSE 'usd' END,CASE WHEN n=202 THEN NULL ELSE 'month' END FROM generate_series(0,202) n`, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id=$1`, "personal:"+account)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, "personal:"+account)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+account)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_products WHERE starts_with(code,$1)`, prefix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	fake := &workspaceStripeFake{}
	service := billing.NewWorkspaceService(store, fake, "price-test-key")
	seen := map[string]bool{}
	total := 0
	for offset := 0; ; {
		page, err := service.Products(ctx, account, "", 40, offset)
		if err != nil {
			t.Fatal(err)
		}
		if total == 0 {
			total = page.Total
		}
		if page.Total != total {
			t.Fatal("unstable inventory total")
		}
		for _, product := range page.Items {
			if seen[product.Code] {
				t.Fatal("duplicate product across pages")
			}
			seen[product.Code] = true
			if product.Code == prefix+"202" {
				if product.PriceVerified || product.UnitAmount != nil || product.Currency != "" {
					t.Fatal("unverified price displayed as zero")
				}
			}
			if product.Code == prefix+"000" {
				if !product.PriceVerified || product.UnitAmount == nil || *product.UnitAmount != 2500 || product.Currency != "usd" || product.Interval != "month" || product.Version != 1 || product.CatalogType != "personal_subscription" {
					t.Fatal("verified price metadata missing")
				}
			}
		}
		offset += len(page.Items)
		if offset >= total {
			break
		}
		if len(page.Items) == 0 {
			t.Fatal("pagination stalled")
		}
	}
	if !seen[prefix+"202"] || len(seen) < 203 {
		t.Fatal("later catalog pages hidden")
	}
	if _, err = service.Checkout(ctx, account, account+"@example.test", "", prefix+"000", "stale_"+suffix, "https://example.test/success", "https://example.test/cancel", 2); !errors.Is(err, billing.ErrProductChanged) || fake.checkouts != 0 {
		t.Fatalf("stale reviewed price reached provider: %v", err)
	}
	if _, err = service.Products(ctx, account, "", 201, 0); !errors.Is(err, billing.ErrInvalidProductPage) {
		t.Fatal("unbounded page accepted")
	}
	if _, err = service.Overview(ctx, account, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET product_code=$2 WHERE id=$1`, "personal:"+account, prefix+"000"); err != nil {
		t.Fatal(err)
	}
	overview, err := service.Overview(ctx, account, "")
	if err != nil || overview.Product == nil || overview.Product.UnitAmount == nil || *overview.Product.UnitAmount != 2500 {
		t.Fatalf("current price absent: %v", err)
	}
}
