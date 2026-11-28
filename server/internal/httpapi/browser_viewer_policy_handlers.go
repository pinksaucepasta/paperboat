package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/browseraccess"
)

// viewerPolicyAuth admits a browser session, or a pb sign-in that holds the access scope of the policy's resource
// kind (previews:read, tunnels:write, …).
func viewerPolicyAuth(users *auth.Service, machines *auth.DeviceService, access string, next http.Handler) http.Handler {
	scoped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var scope string
		switch r.PathValue("resource_kind") {
		case "preview":
			scope = "previews:" + access
		case "tunnel":
			scope = "tunnels:" + access
		default:
			writeError(w, r, http.StatusBadRequest, "invalid_request", "The viewer policy is not valid.")
			return
		}
		requireScope(scope, next).ServeHTTP(w, r)
	})
	if machines == nil {
		return requireAuth(users, scoped)
	}
	return requireAnyAuth(users, machines, scoped)
}

// viewerPolicyActor is the signed-in account of a browser session or a pb sign-in. An API token is not one.
func viewerPolicyActor(r *http.Request) (string, bool) {
	p, ok := principalFromContext(r.Context())
	if !ok || p.APIToken != nil || p.User.ID == "" || p.Client == nil && p.Session.ID == "" {
		return "", false
	}
	return p.User.ID, true
}

func browserViewerPolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == browseraccess.ErrInvalid:
		writeError(w, r, http.StatusBadRequest, "invalid_request", "The viewer policy is not valid.")
	case err == browseraccess.ErrPolicyConflict:
		writeError(w, r, http.StatusConflict, "viewer_policy_conflict", "The viewer policy changed. Reload it and apply the change again.")
	case err == browseraccess.ErrPolicyOwnerChanged:
		writeError(w, r, http.StatusConflict, "viewer_policy_owner_changed", "The resource owner changed since this policy was configured. Reload the policy before changing it.")
	case err == browseraccess.ErrPolicyWidening:
		writeError(w, r, http.StatusConflict, "native_access_widening_denied", "This access cannot be set: a private preview or tunnel can be limited to its owner or to listed email addresses and domains, and a team one to its owner.")
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
	actor, ok := viewerPolicyActor(r)
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_required", "Sign in to view this access policy.")
		return
	}
	policy, err := h.Access.GetViewerPolicy(r.Context(), actor, r.PathValue("resource_kind"), r.PathValue("resource_id"), r.PathValue("route_id"))
	if err != nil {
		browserViewerPolicyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
}

func (h *BrowserAccessHandlers) ViewerPolicyPut(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	actor, ok := viewerPolicyActor(r)
	if !ok {
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
		ActorAccountID: actor, ResourceKind: r.PathValue("resource_kind"),
		ResourceID: r.PathValue("resource_id"), RouteID: r.PathValue("route_id"),
		Audience: body.Audience, TeamID: body.TeamID, AllowedEmails: body.AllowedEmails,
		EmailDomain: body.EmailDomain, ExpectedGeneration: body.ExpectedGeneration,
	})
	if err != nil {
		browserViewerPolicyError(w, r, err)
		return
	}
	if policy.ServingState != "ready" && policy.ServingState != "not_applicable" {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: policy})
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
}
