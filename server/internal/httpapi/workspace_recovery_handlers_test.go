package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

type workspaceRecoveryFunc func(context.Context, string, string, string, string) error

func (f workspaceRecoveryFunc) RetireNotApplied(ctx context.Context, actor, key, operation, evidence string) error {
	return f(ctx, actor, key, operation, evidence)
}
func (f workspaceRecoveryFunc) ReconcileCheckout(ctx context.Context, actor, key, operation, session, evidence string) error {
	return f(ctx, actor, key, operation, evidence)
}
func (f workspaceRecoveryFunc) SupersedePortal(ctx context.Context, actor, key, operation, evidence string) error {
	return f(ctx, actor, key, operation, evidence)
}

func TestWorkspaceRecoveryHTTPProviderActions(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		reconcile        bool
	}{
		{"checkout", "/v1/admin/billing/workspace-operations/op/reconcile-checkout", `{"provider_session_id":"cs_verified","evidence_reference":"case:verified-2"}`, true},
		{"portal", "/v1/admin/billing/workspace-operations/op/supersede-portal", `{"evidence_reference":"case:verified-2"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			f := workspaceRecoveryFunc(func(_ context.Context, actor, key, op, evidence string) error {
				called = true
				if actor != "admin-user" || key != "recovery-key" || op != "op" || evidence != "case:verified-2" {
					t.Fatal("identity changed")
				}
				return nil
			})
			var h http.Handler
			if tc.reconcile {
				h = adminReconcileWorkspaceCheckout(f)
			} else {
				h = adminSupersedeWorkspacePortal(f)
			}
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "recovery-key")
			r.SetPathValue("operation_id", "op")
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "admin-user", Role: auth.RoleAdmin}}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 204 || !called {
				t.Fatalf("status=%d called=%v body=%s", w.Code, called, w.Body.String())
			}
		})
	}
}

func TestWorkspaceRecoveryHTTPAuthorityAndOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       auth.Role
		body       string
		key        string
		csrf       bool
		serviceErr error
		status     int
		called     bool
	}{
		{"anonymous", "", `{"outcome":"not_applied"}`, "recovery-key", false, nil, 401, false},
		{"member", auth.RoleUser, `{"outcome":"not_applied"}`, "recovery-key", false, nil, 403, false},
		{"support", auth.RoleSupport, `{"outcome":"not_applied"}`, "recovery-key", false, nil, 403, false},
		{"missing csrf", auth.RoleAdmin, `{"outcome":"not_applied"}`, "recovery-key", true, nil, 403, false},
		{"missing key", auth.RoleAdmin, `{"outcome":"not_applied"}`, "", false, nil, 400, false},
		{"cannot assert success", auth.RoleAdmin, `{"outcome":"applied","evidence_reference":"case:verified-1"}`, "recovery-key", false, nil, 400, false},
		{"unknown field", auth.RoleAdmin, `{"outcome":"not_applied","secret":"never-accepted"}`, "recovery-key", false, nil, 400, false},
		{"retire", auth.RoleAdmin, `{"outcome":"not_applied","evidence_reference":"case:verified-1"}`, "recovery-key", false, nil, 204, true},
		{"revoked admin", auth.RoleAdmin, `{"outcome":"not_applied","evidence_reference":"case:verified-1"}`, "recovery-key", false, billing.ErrWorkspaceForbidden, 403, true},
		{"active retry window", auth.RoleAdmin, `{"outcome":"not_applied","evidence_reference":"case:verified-1"}`, "recovery-key", false, billing.ErrWorkspaceRecoveryTooEarly, 409, true},
		{"conflicting replay", auth.RoleAdmin, `{"outcome":"not_applied","evidence_reference":"case:verified-1"}`, "recovery-key", false, billing.ErrBillingRecoveryConflict, 409, true},
		{"private error", auth.RoleAdmin, `{"outcome":"not_applied","evidence_reference":"case:verified-1"}`, "recovery-key", false, errors.New("private database details"), 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := adminRetireWorkspaceBillingOperation(workspaceRecoveryFunc(func(_ context.Context, actor, key, operation, evidence string) error {
				called = true
				if actor != "admin-user" || key != "recovery-key" || operation != "original-operation" || evidence != "case:verified-1" {
					t.Fatal("request identity changed")
				}
				return tc.serviceErr
			}))
			if tc.csrf {
				h = requireCSRF(auth.NewService(nil, nil, nil, nil, false), h)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/admin/billing/workspace-operations/original-operation/retire", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", tc.key)
			r.SetPathValue("operation_id", "original-operation")
			if tc.role != "" {
				r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "admin-user", Role: tc.role}}))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || called != tc.called {
				t.Fatalf("status=%d called=%v", w.Code, called)
			}
			if strings.Contains(w.Body.String(), "private database details") {
				t.Fatal("internal error leaked")
			}
		})
	}
}
