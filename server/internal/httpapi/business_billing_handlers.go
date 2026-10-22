package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

// Business billing (PLAN F19-11/12): invoices, billing details and pay by invoice for personal
// (/personal/...) and team (/teams/{team_id}/...) billing workspaces.
func registerBusinessBillingRoutes(mux *http.ServeMux, s *billing.WorkspaceService, read, write func(http.Handler) http.Handler) {
	for _, base := range []string{"/v1/billing/workspaces/personal", "/v1/billing/workspaces/teams/{team_id}"} {
		mux.Handle("GET "+base+"/invoices", read(businessBilling(s, "invoices")))
		mux.Handle("GET "+base+"/details", read(businessBilling(s, "details")))
		mux.Handle("PUT "+base+"/details", write(businessBilling(s, "update_details")))
	}
	mux.Handle("GET /v1/billing/workspaces/teams/{team_id}/pay-by-invoice", read(businessBilling(s, "collection")))
	mux.Handle("PUT /v1/billing/workspaces/teams/{team_id}/pay-by-invoice", write(businessBilling(s, "set_collection")))
}

func businessBilling(s *billing.WorkspaceService, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		team, key := r.PathValue("team_id"), strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		var out any
		var err error
		switch action {
		case "invoices":
			out, err = s.Invoices(r.Context(), p.User.ID, team, r.URL.Query().Get("cursor"))
		case "details":
			out, err = s.BillingDetails(r.Context(), p.User.ID, team)
		case "update_details":
			var in billing.BillingDetails
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.UpdateBillingDetails(r.Context(), p.User.ID, team, in, key)
		case "collection":
			out, err = s.PayByInvoice(r.Context(), p.User.ID, team)
		case "set_collection":
			var in struct {
				PayByInvoice bool `json:"pay_by_invoice"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.SetPayByInvoice(r.Context(), p.User.ID, team, in.PayByInvoice, key)
		}
		var provider billing.StripeAPIError
		switch {
		case err == nil:
			writeJSON(w, 200, SuccessResponse{Data: out})
		case errors.Is(err, billing.ErrInvalidBillingDetails):
			writeError(w, r, 400, "invalid_billing_details", "Check the billing email, company name, address (country as a two-letter code) and tax ID, and send an Idempotency-Key.")
		case errors.As(err, &provider) && provider.StatusCode == http.StatusBadRequest:
			writeError(w, r, 400, "invalid_billing_details", "Stripe refused these details. Check the tax ID type and number and the address.")
		case errors.Is(err, billing.ErrNoBillingCustomer):
			writeError(w, r, 409, "no_billing_account", "This workspace has no billing account yet. Buy a plan first.")
		case errors.Is(err, billing.ErrPayByInvoiceIneligible):
			writeError(w, r, 409, "pay_by_invoice_ineligible", "Pay by invoice needs an active yearly Team plan with enough seats.")
		default:
			workspaceBillingError(w, r, err)
		}
	}
}
