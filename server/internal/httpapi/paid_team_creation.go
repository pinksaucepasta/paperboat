package httpapi

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"net/http"
	"strings"
)

func paidTeamCreation(s *billing.WorkspaceService, origins []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		limitEnvironmentJSON(w, r, 16<<10)
		var in billing.TeamCreationInput
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || key != in.OperationID || !billingReturnAllowed(in.SuccessURL, origins) || !billingReturnAllowed(in.CancelURL, origins) {
			writeError(w, r, 400, "validation_failed", "Provide matching operation and idempotency IDs and configured console return URLs.")
			return
		}
		out, err := s.CreatePaidTeam(r.Context(), p.User.ID, p.User.PrimaryEmail, in)
		switch {
		case errors.Is(err, billing.ErrTeamCreationInvalid):
			writeError(w, r, 400, "validation_failed", "Provide an available team slug, at least one seat and month or year interval.")
		case errors.Is(err, billing.ErrTeamReserved):
			writeError(w, r, 409, "team_reserved", "This team identifier is already reserved. Choose another identifier or retry the original operation.")
		case err != nil:
			workspaceBillingError(w, r, err)
		default:
			writeJSON(w, 200, SuccessResponse{Data: map[string]string{"url": out.URL}})
		}
	}
}
