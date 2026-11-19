package httpapi

import (
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
)

// workspaceAddonChange sets the team's SSO/SCIM add-on ("" removes it) and returns the team's billing overview.
func workspaceAddonChange(s *billing.WorkspaceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		var in struct {
			Addon              string `json:"addon"`
			AcceptRefundPolicy bool   `json:"accept_refund_policy"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 200 {
			writeError(w, r, 400, "validation_failed", "Provide an Idempotency-Key for this add-on change.")
			return
		}
		out, err := s.ChangeAddon(r.Context(), p.User.ID, r.PathValue("team_id"), in.Addon, in.AcceptRefundPolicy, key)
		if err != nil {
			workspaceBillingError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}
