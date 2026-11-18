package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorreporting"
	"github.com/pinksaucepasta/paperboat/server/internal/posthogquery"
	"github.com/pinksaucepasta/paperboat/server/internal/revenue"
)

type downStripe struct{}

func (downStripe) Get(context.Context, string, any) error {
	return billing.StripeAPIError{StatusCode: 503}
}

type noRecords struct{}

func (noRecords) PlanCodes(context.Context) (map[string]string, error)            { return nil, nil }
func (noRecords) Workspaces(context.Context, []string) (map[string]string, error) { return nil, nil }

func serveMoney(t *testing.T, s *revenue.Service, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /v1/admin/money", moneyRead(func(w http.ResponseWriter, r *http.Request) (any, error) { return s.Report(r.Context(), 30) }))
	mux.Handle("GET /v1/admin/money/events", moneyRead(func(w http.ResponseWriter, r *http.Request) (any, error) { return s.Events(r.Context(), "", 5) }))
	mux.Handle("GET /v1/admin/money/lists/{list}", moneyRead(func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.List(r.Context(), r.PathValue("list"), "", 5)
	}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w
}

func TestMoneyRoutesAreForBillingStaffAndSayWhenNotConfigured(t *testing.T) {
	for _, route := range []string{"GET /v1/admin/money", "GET /v1/admin/money/lists/{list}", "GET /v1/admin/money/events"} {
		if operatorPolicies[route].capability != "billing" {
			t.Errorf("%s must need the billing capability", route)
		}
	}
	for role, want := range map[auth.Role]bool{auth.RoleAdmin: true, auth.RoleBilling: true, auth.RoleSupport: false, auth.RoleOperations: false, auth.RoleUser: false} {
		if got := operatorAllowed(role, "billing"); got != want {
			t.Errorf("billing for %s = %v, want %v", role, got, want)
		}
	}
	for _, path := range []string{"/v1/admin/money", "/v1/admin/money/events", "/v1/admin/money/lists/customers"} {
		w := serveMoney(t, revenue.New(nil, nil), path)
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"money_not_configured"`) {
			t.Errorf("%s unset: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestMoneyDownIsDistinctFromNotConfigured(t *testing.T) {
	down := revenue.New(downStripe{}, noRecords{})
	for _, path := range []string{"/v1/admin/money", "/v1/admin/money/events", "/v1/admin/money/lists/customers"} {
		w := serveMoney(t, down, path)
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"money_unavailable"`) || strings.Contains(w.Body.String(), "not_configured") {
			t.Errorf("%s down: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// Every optional operator integration answers "not configured" and "down" with different codes, so the panel can say
// "not set up" in one line for the first and show a short error with a reference for the second.
func TestOptionalIntegrationsAnswerNotConfiguredAndDownDistinctly(t *testing.T) {
	read := func(t *testing.T, h http.Handler) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
		return w.Code, w.Body.String()
	}
	for _, tc := range []struct {
		name        string
		unset, down http.Handler
		unsetCode   string
		downCode    string
	}{
		{"Stripe", moneyRead(func(http.ResponseWriter, *http.Request) (any, error) { return nil, revenue.ErrNotConfigured }),
			moneyRead(func(http.ResponseWriter, *http.Request) (any, error) {
				return nil, errors.Join(revenue.ErrProvider, errors.New("boom"))
			}),
			"money_not_configured", "money_unavailable"},
		{"PostHog", analyticsRead(nil, siteRead),
			analyticsRead(posthogquery.New(posthogquery.Config{Host: "https://127.0.0.1:1", ProjectID: "1", Key: "k"}), siteRead),
			"analytics_not_configured", "analytics_unavailable"},
		{"Sentry", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			operatorDiagnosticsError(w, r, operatorreporting.ErrDisabled)
		}),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				operatorDiagnosticsError(w, r, errors.New("sentry timed out"))
			}),
			"sentry_not_configured", "operator_evidence_unavailable"},
	} {
		code, body := read(t, tc.unset)
		if code != 503 || !strings.Contains(body, `"`+tc.unsetCode+`"`) {
			t.Errorf("%s unset: %d %s", tc.name, code, body)
		}
		code, body = read(t, tc.down)
		if code != 503 || !strings.Contains(body, `"`+tc.downCode+`"`) || strings.Contains(body, "not_configured") {
			t.Errorf("%s down: %d %s", tc.name, code, body)
		}
	}
}
