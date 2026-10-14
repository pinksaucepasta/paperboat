package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func browserViewerPolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == browseraccess.ErrInvalid:
		writeError(w, r, http.StatusBadRequest, "invalid_request", "The viewer policy is not valid.")
	case err == browseraccess.ErrPolicyConflict:
		writeError(w, r, http.StatusConflict, "viewer_policy_conflict", "The viewer policy changed. Reload it and apply the change again.")
	case err == browseraccess.ErrPolicyOwnerChanged:
		writeError(w, r, http.StatusConflict, "viewer_policy_owner_changed", "The resource owner changed since this policy was configured. Reload the policy before changing it.")
	case err == browseraccess.ErrPolicyWidening:
		writeError(w, r, http.StatusConflict, "native_access_widening_denied", "The viewer policy cannot grant access beyond the tunnel's native private or team access.")
	case err == browseraccess.ErrPolicyEdgeTransition:
		writeError(w, r, http.StatusConflict, "edge_policy_transition_required", "No viewer policy change was saved. Current edge assignments do not safely support this public or restricted transition yet.")
	case err == browseraccess.ErrNotAuthorized:
		writeError(w, r, http.StatusNotFound, "not_found", "Resource not found.")
	default:
		writeError(w, r, http.StatusServiceUnavailable, "viewer_policy_unavailable", "Viewer policy could not be loaded or saved. Retry after the service recovers.", err)
	}
}

func (h *BrowserAccessHandlers) ViewerPolicyGet(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok || p.Client != nil || p.User.ID == "" || p.Session.ID == "" {
		writeError(w, r, http.StatusUnauthorized, "authentication_required", "Sign in to view this access policy.")
		return
	}
	policy, err := h.Access.GetViewerPolicy(r.Context(), p.User.ID, r.PathValue("resource_kind"), r.PathValue("resource_id"), r.PathValue("route_id"))
	if err != nil {
		browserViewerPolicyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
}

func (h *BrowserAccessHandlers) ViewerPolicyPut(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok || p.Client != nil || p.User.ID == "" || p.Session.ID == "" {
		writeError(w, r, http.StatusUnauthorized, "authentication_required", "Sign in to change this access policy.")
		return
	}
	raw, err := browserBody(w, r)
	if err != nil {
		browserViewerPolicyError(w, r, err)
		return
	}
	var body struct {
		Audience           string   `json:"audience"`
		TeamID             string   `json:"team_id,omitempty"`
		AllowedEmails      []string `json:"allowed_emails,omitempty"`
		EmailDomain        string   `json:"email_domain,omitempty"`
		ExpectedGeneration int64    `json:"expected_generation"`
	}
	if err = browserDecode(raw, &body); err != nil {
		browserViewerPolicyError(w, r, err)
		return
	}
	policy, err := h.Access.SetViewerPolicy(r.Context(), browseraccess.SetViewerPolicyRequest{
		ActorAccountID: p.User.ID, ResourceKind: r.PathValue("resource_kind"),
		ResourceID: r.PathValue("resource_id"), RouteID: r.PathValue("route_id"),
		Audience: body.Audience, TeamID: body.TeamID, AllowedEmails: body.AllowedEmails,
		EmailDomain: body.EmailDomain, ExpectedGeneration: body.ExpectedGeneration,
	})
	if err != nil {
		browserViewerPolicyError(w, r, err)
		return
	}
	if h.ViewerPolicyReconciler != nil && policy.ResourceKind == "tunnel" {
		// The policy is already durable. Reconcile immediately after commit so
		// the edge sees the effective mode/generation without waiting for the
		// periodic worker. A failed/pending edge transition remains represented
		// in serving_state; it must not be reported as an unsaved conflict.
		_, reconcileErr := h.ViewerPolicyReconciler.ReconcileTunnelEdgeRouteAssignments(r.Context(), 100)
		current, readErr := h.Access.GetViewerPolicy(r.Context(), p.User.ID, policy.ResourceKind, policy.ResourceID, policy.RouteID)
		if readErr == nil {
			policy = current
		}
		if reconcileErr != nil || readErr != nil {
			code := "edge_route_reconcile_failed"
			cause := reconcileErr
			if readErr != nil {
				if cause == nil {
					code = "viewer_policy_unavailable"
					cause = readErr
				} else {
					cause = errors.Join(cause, readErr)
				}
			}
			ctx := supportreport.WithFailureIdentity(r.Context(), p.User.ID, requestIDFromContext(r.Context()), supportReference(r.Context()))
			supportreport.ObserveFromContext(ctx, "paperboat-server", supportOperation(r.URL.Path), "service_error", code, cause)
		}
	}
	if policy.ServingState != "ready" && policy.ServingState != "not_applicable" {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: policy})
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
}
