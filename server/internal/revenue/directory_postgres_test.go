package revenue_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/revenue"
)

// The money page links a Stripe customer to our billing workspace and names a price by its plan code.
func TestRecordsMapStripeIdsToWorkspacesAndPlans(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run revenue integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	user, workspace, customer, price, code := "usr_rev_"+suffix, "ws_rev_"+suffix, "cus_rev_"+suffix, "price_rev_"+suffix, "rev_plan_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test")
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,state) VALUES($1,$2,$3,'active')`, workspace, user, customer)
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$2,$3,'personal_subscription','{}',true)`, code, "prod_"+suffix, price)
	t.Cleanup(func() {
		store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
		store.SQL().ExecContext(ctx, `DELETE FROM paperboat.billing_products WHERE id=$1`, code)
		store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	records := revenue.Records{Store: store}
	plans, err := records.PlanCodes(ctx)
	if err != nil || plans[price] != code {
		t.Fatalf("plans %v %v", plans[price], err)
	}
	ws, err := records.Workspaces(ctx, []string{customer, "cus_unknown_" + suffix})
	if err != nil || len(ws) != 1 || ws[customer] != workspace {
		t.Fatalf("workspaces %v %v", ws, err)
	}
	if none, err := records.Workspaces(ctx, nil); err != nil || len(none) != 0 {
		t.Fatalf("no customers: %v %v", none, err)
	}
}
