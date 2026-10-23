package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestWorkspaceBillingReturnOrigin(t *testing.T) {
	origins := []string{"https://dashboard.example.test", "http://100.64.0.1:3000"}
	for _, raw := range []string{"https://dashboard.example.test/dashboard/subscriptions", "http://100.64.0.1:3000/dashboard/teams?paid=1"} {
		if !billingReturnAllowed(raw, origins) {
			t.Errorf("valid dashboard return denied: %s", raw)
		}
	}
	for _, raw := range []string{"https://dashboard.example.test.evil.test/", "https://dashboard.example.test@evil.test/", "https://user@dashboard.example.test/", "https://dashboard.example.test:444/", "//dashboard.example.test/", "javascript:alert(1)", "https://dashboard.example.test/#secret"} {
		if billingReturnAllowed(raw, origins) {
			t.Errorf("unsafe return accepted: %s", raw)
		}
	}
}

// The real overview must expose the budget shared by historical segments, not
// the provider renewal or the current segment's clipped start.
func TestWorkspaceAllowancePeriodsPostgres(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	t.Cleanup(func() { resetIntegrationTables(t, store) })
	ctx := t.Context()
	cookies := loginCookies(t, router, "allowance_period:allowance-period@example.test:Allowance")
	var user string
	if err := store.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.users WHERE workos_subject='allowance_period'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := month.AddDate(0, 1, 0)
	switched := month.Add(now.Sub(month) / 2)
	get := func(wantStart, wantEnd time.Time, wantUsage int64) {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/billing/workspaces/personal", nil)
		addCookies(req, cookies)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("overview status=%d", rec.Code)
		}
		var response struct {
			Data struct {
				AllowancePeriodStart time.Time  `json:"allowance_period_start"`
				AllowancePeriodEnd   time.Time  `json:"allowance_period_end"`
				PeriodStart          *time.Time `json:"period_start"`
				PeriodEnd            *time.Time `json:"period_end"`
				UsageBytes           int64      `json:"usage_bytes"`
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.Data.AllowancePeriodStart.Equal(wantStart) || !response.Data.AllowancePeriodEnd.Equal(wantEnd) || response.Data.UsageBytes != wantUsage {
			t.Fatalf("allowance period=%s..%s usage=%d; want %s..%s usage=%d", response.Data.AllowancePeriodStart, response.Data.AllowancePeriodEnd, response.Data.UsageBytes, wantStart, wantEnd, wantUsage)
		}
		if wantUsage > 0 && (response.Data.PeriodStart == nil || !response.Data.PeriodStart.Equal(switched) || response.Data.PeriodEnd == nil || !response.Data.PeriodEnd.Equal(now.AddDate(1, 0, 0))) {
			t.Fatal("annual provider renewal changed in overview")
		}
		for _, updated := range rec.Result().Cookies() {
			for i, old := range cookies {
				if old.Name == updated.Name {
					cookies[i] = updated
				}
			}
		}
	}
	get(month, nextMonth, 0) // Free calendar budget.
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_interval) VALUES ('allowance-plan','allowance-plan','stripe','allowance-product','allowance-price','personal_subscription','{"allowance_bytes":100}',true,'month')`)
	exec(`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, user)
	exec(`UPDATE paperboat.billing_workspaces SET state='active',product_code='allowance-plan',period_start=$2,period_end=$3,bandwidth_anchor=$2 WHERE user_id=$1`, user, month, nextMonth)
	get(month, nextMonth, 0) // Monthly subscription.
	service := billing.NewWorkspaceService(store, nil, "")
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, _, err := service.DebitWorkspaceBandwidthTx(ctx, tx, "personal:"+user, 7, month.Add(switched.Sub(month)/2))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A portal switch changes provider dates and splits the paid segment. Both
	// segments still spend the same original monthly budget.
	partialEnd := now.Add(nextMonth.Sub(now) / 2)
	exec(`UPDATE paperboat.billing_workspace_usage_periods SET period_end=$2 WHERE workspace_id='personal:'||$1 AND period_start=$3`, user, switched, month)
	exec(`INSERT INTO paperboat.billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,paid,budget_start) VALUES('personal:'||$1,$2,$3,100,true,$4)`, user, switched, partialEnd, month)
	exec(`UPDATE paperboat.billing_workspaces SET period_start=$2,period_end=$3 WHERE user_id=$1`, user, switched, now.AddDate(1, 0, 0))
	exec(`UPDATE paperboat.billing_products SET verified_interval='year' WHERE code='allowance-plan'`)
	get(month, nextMonth, 7)
	// An annual Jan31 anchor must return to day31 after a short month; an
	// independent PostgreSQL calendar calculation supplies the expected bounds.
	anchor := time.Date(now.Year(), time.January, 31, 12, 0, 0, 0, time.UTC)
	if anchor.After(now) {
		anchor = anchor.AddDate(-1, 0, 0)
	}
	exec(`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, user)
	exec(`UPDATE paperboat.billing_workspaces SET period_start=$2,period_end=$3,bandwidth_anchor=$2 WHERE user_id=$1`, user, anchor, anchor.AddDate(1, 0, 0))
	var start, end time.Time
	if err := store.SQL().QueryRowContext(ctx, `WITH bounds AS (SELECT date_trunc('month',now() AT TIME ZONE 'UTC') AS m), clock AS (SELECT m,(m+interval '1 month -1 day 12 hours') AT TIME ZONE 'UTC' AS boundary FROM bounds) SELECT CASE WHEN now()<boundary THEN (m+interval '-1 day 12 hours') AT TIME ZONE 'UTC' ELSE boundary END,CASE WHEN now()<boundary THEN boundary ELSE (m+interval '2 months -1 day 12 hours') AT TIME ZONE 'UTC' END FROM clock`).Scan(&start, &end); err != nil {
		t.Fatal(err)
	}
	get(start, end, 0)
}
