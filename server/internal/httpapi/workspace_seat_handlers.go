package httpapi

import (
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"net/http"
	"strconv"
	"strings"
)

func workspaceSeatChange(s *billing.WorkspaceService, preview bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		var quantity int
		if preview {
			var err error
			quantity, err = strconv.Atoi(r.URL.Query().Get("quantity"))
			if err != nil {
				writeError(w, r, 400, "validation_failed", "Provide a whole seat quantity.")
				return
			}
		} else {
			var in struct {
				Quantity int `json:"quantity"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			quantity = in.Quantity
		}
		var out billing.SeatPreview
		var err error
		if preview {
			out, err = s.PreviewSeats(r.Context(), p.User.ID, r.PathValue("team_id"), quantity)
		} else {
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if key == "" || len(key) > 200 {
				writeError(w, r, 400, "validation_failed", "Provide an Idempotency-Key for this seat change.")
				return
			}
			out, err = s.ChangeSeats(r.Context(), p.User.ID, r.PathValue("team_id"), quantity, key)
		}
		if err != nil {
			workspaceBillingError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}
