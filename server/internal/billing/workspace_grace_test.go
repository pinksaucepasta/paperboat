package billing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestRenewalGraceSuspendsAndRecoversWithoutRetryExtension(t *testing.T) {
	for _, scope := range []string{"team", "personal"} {
		t.Run(scope, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			suffix := fmt.Sprint(time.Now().UnixNano())
			user, team, code := "usr_grace_"+suffix, "grace_team_"+suffix, "grace_plan_"+suffix
			workspace := "personal:" + user
			insertUser(t, store, user, user+"@example.test")
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			if scope == "team" {
				workspace = "team:" + team
				exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
				exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, user)
			}
			t.Cleanup(func() {
				exec(`DELETE FROM paperboat.stripe_events WHERE id LIKE $1`, "evt_grace_%"+suffix)
				exec(`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1`, workspace)
				exec(`DELETE FROM paperboat.billing_workspaces WHERE id=$1`, workspace)
				exec(`DELETE FROM paperboat.billing_products WHERE code=$1`, code)
				exec(`DELETE FROM paperboat.teams WHERE team_id=$1`, team)
				exec(`DELETE FROM paperboat.users WHERE id=$1`, user)
			})
			exec(`INSERT INTO paperboat.billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active) VALUES($1,$1,'stripe',$1,$1,$2,'{"allowance_bytes":100}',true)`, code, scope+"_subscription")
			now := time.Now().UTC().Truncate(time.Second)
			start := now.Add(-time.Hour)
			end := start.AddDate(0, 1, 0)
			userID, teamID := user, ""
			if scope == "team" {
				userID, teamID = "", team
			}
			exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,team_id,provider_customer_id,provider_subscription_id,state,product_code,period_start,period_end) VALUES($1,NULLIF($2,''),NULLIF($3,''),$4,$5,'active',$6,$7,$8)`, workspace, userID, teamID, "cus_"+suffix, "sub_"+suffix, code, start, end)
			var mu sync.Mutex
			var response []byte
			snapshot := func(status, reason string) {
				t.Helper()
				mu.Lock()
				defer mu.Unlock()
				response, _ = json.Marshal(map[string]any{"id": "sub_" + suffix, "customer": "cus_" + suffix, "status": status, "latest_invoice": map[string]any{"id": "in_" + suffix, "billing_reason": reason, "status": "open", "attempted": true, "amount_remaining": 800, "status_transitions": map[string]any{"finalized_at": start.Unix()}}, "items": map[string]any{"data": []any{map[string]any{"id": "si_" + suffix, "quantity": 1, "current_period_start": start.Unix(), "current_period_end": end.Unix(), "price": map[string]any{"id": code}}}}})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions/sub_"+suffix || r.URL.Query().Get("expand[]") != "latest_invoice" {
					http.Error(w, "subscription invoice expansion required", http.StatusBadRequest)
					return
				}
				mu.Lock()
				body := append([]byte(nil), response...)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			service := billing.NewWorkspaceService(store, billing.HTTPStripeClient{BaseURL: server.URL, APIKey: "sk_test_fixture", Client: server.Client()}, "")
			apply := func(label, status, reason string, attempt int, created time.Time) {
				t.Helper()
				snapshot(status, reason)
				raw := fmt.Sprintf(`{"id":%q,"type":"invoice.payment_failed","created":%d,"data":{"object":{"id":%q,"object":"invoice","billing_reason":%q,"attempt_count":%d,"parent":{"type":"subscription_details","subscription_details":{"subscription":%q}}}}}`, "evt_grace_"+label+suffix, created.Unix(), "in_"+suffix, reason, attempt, "sub_"+suffix)
				event, err := billing.ParseStripeWebhook([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				if processed, err := service.HandleWebhook(ctx, event); err != nil || !processed {
					t.Fatal("webhook not reconciled", err)
				}
			}
			entitled := func(want bool) {
				t.Helper()
				if scope != "team" {
					return
				}
				var got bool
				if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.team_paid_entitled($1)`, team).Scan(&got); err != nil || got != want {
					t.Fatalf("team paid benefits=%v want=%v err=%v", got, want, err)
				}
			}
			entitled(true)
			failure := start.Add(time.Minute)
			// A subscription update/retry may arrive before the first failure
			// event; its authoritative invoice clock already bounds grace.
			apply("observed", "past_due", "subscription_cycle", 2, failure.Add(time.Hour))
			entitled(true)
			apply("first", "past_due", "subscription_cycle", 1, failure)
			entitled(true)
			var deadline time.Time
			if err := store.SQL().QueryRowContext(ctx, `SELECT renewal_grace_until FROM paperboat.billing_workspaces WHERE id=$1`, workspace).Scan(&deadline); err != nil || !deadline.Equal(failure.Add(7*24*time.Hour)) {
				t.Fatal("renewal grace is not seven days from first failure", err)
			}
			overview, err := service.Overview(ctx, user, teamID)
			if err != nil || overview.RenewalGraceUntil == nil || !overview.RenewalGraceUntil.Equal(deadline) || overview.PaidBenefitsUntil == nil || !overview.PaidBenefitsUntil.Equal(deadline) {
				t.Fatal("workspace overview omitted the authoritative grace deadline", err)
			}
			apply("retry", "past_due", "subscription_cycle", 2, failure.Add(48*time.Hour))
			var repeated time.Time
			if err := store.SQL().QueryRowContext(ctx, `SELECT renewal_grace_until FROM paperboat.billing_workspaces WHERE id=$1`, workspace).Scan(&repeated); err != nil || !repeated.Equal(deadline) {
				t.Fatal("retry moved grace deadline", err)
			}
			checkGraceBoundary(t, store, service, workspace, deadline)
			exec(`UPDATE paperboat.billing_workspaces SET renewal_grace_until=now()-interval '1 second' WHERE id=$1`, workspace)
			entitled(false)
			apply("paid", "active", "subscription_cycle", 2, now)
			entitled(true)
			var grace *time.Time
			if err := store.SQL().QueryRowContext(ctx, `SELECT renewal_grace_until FROM paperboat.billing_workspaces WHERE id=$1`, workspace).Scan(&grace); err != nil || grace != nil {
				t.Fatal("recovered payment retained grace", err)
			}
			// A failed initial purchase or seat-change invoice must not create renewal grace.
			apply("seat", "past_due", "subscription_update", 1, now)
			entitled(false)
			if err := store.SQL().QueryRowContext(ctx, `SELECT renewal_grace_until FROM paperboat.billing_workspaces WHERE id=$1`, workspace).Scan(&grace); err != nil || grace != nil {
				t.Fatal("non-renewal invoice granted grace", err)
			}
		})
	}
}

func checkGraceBoundary(t *testing.T, store *db.DB, service *billing.WorkspaceService, id string, deadline time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range []struct {
		at   time.Time
		paid bool
	}{{deadline.Add(-time.Second), true}, {deadline, false}} {
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, err := service.CheckWorkspaceBandwidthTx(ctx, tx, id, tc.at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var paid bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT paid FROM paperboat.billing_workspace_usage_periods WHERE workspace_id=$1 AND period_start<=$2 AND period_end>$2`, id, tc.at).Scan(&paid); err != nil || paid != tc.paid {
			t.Fatalf("paid allowance at grace boundary=%v want=%v err=%v", paid, tc.paid, err)
		}
	}
}
