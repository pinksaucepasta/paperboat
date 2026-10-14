package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

func workspaceBillingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, billing.ErrWorkspaceForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "Your current workspace role does not permit this billing action.")
	case errors.Is(err, billing.ErrProductChanged):
		writeError(w, r, http.StatusConflict, "product_changed", "This price changed. Reload the plan and review its price before Checkout.")
	case errors.Is(err, billing.ErrUnknownProduct):
		writeError(w, r, http.StatusBadRequest, "product_unavailable", "This subscription is not configured for this workspace.")
	case errors.Is(err, billing.ErrIdempotencyConflict):
		writeError(w, r, http.StatusConflict, "idempotency_key_conflict", "This operation ID was already used for a different billing request.")
	case errors.Is(err, billing.ErrCheckoutPending):
		writeError(w, r, http.StatusConflict, "billing_pending", "A billing operation is pending. Retry with the same operation ID.")
	case errors.Is(err, billing.ErrCheckoutSessionExpired):
		writeError(w, r, http.StatusConflict, "checkout_expired", "The checkout session expired without completion. You can start a new checkout.")
	case errors.Is(err, billing.ErrWorkspaceOperationFailed):
		writeError(w, r, http.StatusConflict, "billing_operation_failed", "The original billing operation definitively failed. Start a new operation with a new operation ID.")
	case errors.Is(err, billing.ErrWorkspaceSubscriptionExists):
		writeError(w, r, http.StatusConflict, "subscription_exists", "This workspace already has a subscription. Use Manage billing to change it or resolve payment issues.")
	case errors.Is(err, billing.ErrStripeOperationExpired):
		writeError(w, r, http.StatusConflict, "billing_reconciliation_required", "The original payment operation exceeded the provider's safe retry window. Billing support must reconcile it before another operation starts.")
	case errors.Is(err, billing.ErrProviderOutcomeUnknown):
		writeError(w, r, http.StatusServiceUnavailable, "provider_outcome_unknown", "The payment provider outcome is not confirmed. Keep the same operation ID; do not start another purchase.", err)
	default:
		writeError(w, r, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable. No subscription change has been confirmed.", err)
	}
}

func workspaceBillingOverview(s *billing.WorkspaceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		result, err := s.Overview(r.Context(), p.User.ID, r.PathValue("team_id"))
		if err != nil {
			workspaceBillingError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: result})
	}
}

func workspaceBillingProducts(s *billing.WorkspaceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		query, valid := readInventoryQuery(w, r, nil, false)
		if !valid {
			return
		}
		if query.Q != "" || query.Owner != "" {
			writeError(w, r, 400, "invalid_inventory_query", "Billing products support limit and offset.")
			return
		}
		result, err := s.Products(r.Context(), p.User.ID, r.PathValue("team_id"), query.Limit, query.Offset)
		if err != nil {
			workspaceBillingError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"items": result.Items, "pagination": inventoryPagination(query.Limit, query.Offset, result.Total, len(result.Items))}})
	}
}

// Redirects are restricted to configured dashboard origins, independently of the
// caller's resource authority. Stripe secrets and sessions never enter logs.
func billingReturnAllowed(raw string, origins []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	for _, origin := range origins {
		if strings.TrimRight(origin, "/") == u.Scheme+"://"+u.Host {
			return true
		}
	}
	return false
}

func workspaceBillingMutation(s *billing.WorkspaceService, origins []string, portal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		var in struct {
			ProductCode    string `json:"product_code"`
			ProductVersion int64  `json:"product_version"`
			SuccessURL     string `json:"success_url"`
			CancelURL      string `json:"cancel_url"`
			ReturnURL      string `json:"return_url"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		valid := key != "" && len(key) <= 200
		if portal {
			valid = valid && billingReturnAllowed(in.ReturnURL, origins)
		} else {
			valid = valid && in.ProductCode != "" && in.ProductVersion > 0 && billingReturnAllowed(in.SuccessURL, origins) && billingReturnAllowed(in.CancelURL, origins)
		}
		if !valid {
			writeError(w, r, 400, "validation_failed", "Provide an operation ID, an available product for checkout, and configured dashboard return URLs.")
			return
		}
		if portal {
			out, err := s.Portal(r.Context(), p.User.ID, r.PathValue("team_id"), key, in.ReturnURL)
			if err != nil {
				workspaceBillingError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: map[string]string{"url": out.URL}})
			return
		}
		out, err := s.Checkout(r.Context(), p.User.ID, p.User.PrimaryEmail, r.PathValue("team_id"), in.ProductCode, key, in.SuccessURL, in.CancelURL, in.ProductVersion)
		if err != nil {
			workspaceBillingError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]string{"url": out.URL}})
	}
}

func stripeWorkspaceWebhook(s *billing.WorkspaceService, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			writeError(w, r, 503, "billing_unavailable", "Payment webhook configuration is unavailable.")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeError(w, r, 400, "invalid_request", "Webhook body is invalid or too large.")
			return
		}
		if err = billing.VerifyStripeWebhookSignatureAt(body, r.Header.Get("Stripe-Signature"), []string{secret}, 5*time.Minute, time.Now().UTC()); err != nil {
			writeError(w, r, 401, "invalid_signature", "Webhook signature is invalid.")
			return
		}
		event, err := billing.ParseStripeWebhook(body)
		if err != nil {
			writeError(w, r, 400, "invalid_request", "Webhook event is invalid.")
			return
		}
		inserted, err := s.HandleWebhook(r.Context(), event)
		if err != nil {
			workspaceBillingError(w, r, err)
			return
		}
		status := "duplicate"
		if inserted {
			status = "processed"
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]string{"status": status}})
	}
}
