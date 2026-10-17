package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type seatHTTPProvider struct {
	billing.StripeClient
	changes     int
	failPayment bool
}

func (f *seatHTTPProvider) PreviewSeats(_ context.Context, in billing.SeatChange) (billing.SeatPreview, error) {
	return billing.SeatPreview{AmountDueNow: 750, Currency: "usd", NextInvoiceAmount: 4500, NextInvoiceAt: in.PeriodEnd}, nil
}
func (f *seatHTTPProvider) ChangeSeats(_ context.Context, in billing.SeatChange, _ string) (billing.SeatPreview, error) {
	if f.failPayment {
		return billing.SeatPreview{}, billing.StripeAPIError{StatusCode: 402}
	}
	f.changes++
	return in.Preview, nil
}

func TestPurchasedSeatHTTPContractPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	x := time.Now().UTC().Format("20060102150405.000000000")
	owner, admin, team := "seat_http_owner_"+x, "seat_http_admin_"+x, "seat-http-"+strings.ReplaceAll(x, ".", "")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@example.test','active' FROM unnest($1::text[]) x`, []string{owner, admin})
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true)`, team, owner, admin)
	exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,'price_'||$1,'team_subscription','{}',true)`, team)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,product_code,provider_subscription_id,provider_customer_id,provider_item_id,period_start,period_end) VALUES('team:'||$1,$1,'active',2,$1,'sub_'||$1,'cus_'||$1,'si_'||$1,now()-interval '1 day',now()+interval '29 days')`, team)
	provider := &seatHTTPProvider{}
	service := billing.NewWorkspaceService(store, provider, "seat-http-key")
	// Match application startup before the overview creates its allowance window.
	if err = service.InitializeAllowancePolicy(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/billing/workspaces/teams/{team_id}/seats/preview", workspaceSeatChange(service, true))
	mux.Handle("POST /v1/billing/workspaces/teams/{team_id}/seats", workspaceSeatChange(service, false))
	mux.Handle("GET /v1/billing/workspaces/teams/{team_id}", workspaceBillingOverview(service))
	request := func(method, suffix, body, actor, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/billing/workspaces/teams/"+team+suffix, strings.NewReader(body))
		r.Header.Set("Idempotency-Key", key)
		if actor != "" {
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: actor}}))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		method, path, body, actor, key string
		status                         int
	}{
		{"GET", "/seats/preview?quantity=3", "", "", "", 401},
		{"GET", "/seats/preview?quantity=three", "", owner, "", 400},
		{"GET", "/seats/preview?quantity=3", "", admin, "", 403},
		{"POST", "/seats", `{"quantity":3}`, owner, "", 400},
		{"POST", "/seats", `{"quantity":3,"extra":true}`, owner, "invalid" + x, 400},
		{"POST", "/seats", `{"quantity":1}`, owner, "used" + x, 409},
	} {
		w := request(tc.method, tc.path, tc.body, tc.actor, tc.key)
		if w.Code != tc.status {
			t.Fatalf("%s %s status=%d want=%d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	provider.failPayment = true
	if w := request("POST", "/seats", `{"quantity":3}`, owner, "payment"+x); w.Code != 402 {
		t.Fatalf("payment failure status=%d", w.Code)
	}
	if w := request("POST", "/seats", `{"quantity":3}`, owner, "payment"+x); w.Code != 409 {
		t.Fatalf("failed payment replay status=%d", w.Code)
	}
	provider.failPayment = false
	for _, method := range []string{"GET", "POST", "POST"} {
		path, body, key := "/seats/preview?quantity=3", "", ""
		if method == "POST" {
			path, body, key = "/seats", `{"quantity":3}`, "buy"+x
		}
		w := request(method, path, body, owner, key)
		if w.Code != 200 {
			t.Fatalf("seat request status=%d", w.Code)
		}
		var response struct{ Data map[string]any }
		if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Data) != 4 || response.Data["amount_due_now"] != float64(750) || response.Data["currency"] != "usd" || response.Data["next_invoice_amount"] != float64(4500) || response.Data["next_invoice_at"] == nil {
			t.Fatal("seat response contract mismatch")
		}
	}
	if provider.changes != 1 {
		t.Fatalf("replayed HTTP purchase charged %d times", provider.changes)
	}
	w := request("GET", "", "", owner, "")
	var overview struct{ Data map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &overview) != nil || overview.Data["seats_bought"] != float64(3) || overview.Data["seats_used"] != float64(2) {
		t.Fatalf("overview seat contract status=%d", w.Code)
	}
}
