package httpapi

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"math"
	"net/http"
	"strconv"
	"time"
)

type teamInvitationAPI interface {
	InvitationInventory(context.Context, string, string, string, string, string, string, int, int) ([]teams.InvitationSummary, int, error)
	Invitation(context.Context, string, string) (teams.InvitationSummary, error)
	ResendInvitation(context.Context, string, string, string, teams.AcceptRequest) (teams.InvitationSummary, error)
	DeclineInvitation(context.Context, string, string, teams.AcceptRequest) (teams.InvitationSummary, error)
}

func registerTeamInvitationRoutes(mux *http.ServeMux, service teamAPI, read, write func(http.Handler) http.Handler) {
	discovery, ok := service.(teamInvitationAPI)
	if !ok {
		return
	}
	for _, pattern := range []string{"GET /v1/team-invitations", "GET /v1/teams/{team_id}/invitations"} {
		mux.Handle(pattern, read(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			account, ok := vaultScopePrincipal(w, r)
			if !ok {
				return
			}
			extraKeys := []string{}
			if r.PathValue("team_id") == "" {
				extraKeys = append(extraKeys, "direction")
			}
			query, ok := readInventoryQuery(w, r, []string{"pending", "accepted", "cancelled", "expired"}, false, extraKeys...)
			if !ok {
				return
			}
			direction := r.URL.Query().Get("direction")
			if direction == "" {
				direction = "received"
			}
			if direction != "received" && direction != "sent" {
				writeError(w, r, 400, "invalid_request", "Direction must be received or sent.")
				return
			}
			items, total, err := discovery.InvitationInventory(r.Context(), account, r.PathValue("team_id"), query.Q, query.State, query.Owner, direction, query.Limit, query.Offset)
			if err != nil {
				writeError(w, r, 503, "invitations_unavailable", "Invitations could not be loaded.", err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: map[string]any{"items": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
		})))
	}
	mux.Handle("POST /v1/teams/{team_id}/invitations/{invitation_id}/resend", write(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 4096)
		var request teams.AcceptRequest
		if !decodeStrictJSON(w, r, &request) {
			return
		}
		item, err := discovery.ResendInvitation(r.Context(), account, r.PathValue("team_id"), r.PathValue("invitation_id"), request)
		var retry *teams.RetryAfterError
		switch {
		case errors.As(err, &retry):
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(float64(retry.After)/float64(time.Second)))))
			writeError(w, r, 429, "invitation_rate_limited", "Wait before resending this invitation.")
		case errors.Is(err, teams.ErrForbidden):
			writeError(w, r, 403, "forbidden", "A current team administrator is required; only owners can resend admin invitations.")
		case errors.Is(err, teams.ErrNotFound):
			writeError(w, r, 404, "not_found", "Invitation was not found in this team.")
		case errors.Is(err, teams.ErrInvalid):
			writeError(w, r, 400, "invalid_request", "Provide a valid operation_id.")
		case errors.Is(err, teams.ErrSuspended):
			writeError(w, r, 409, "team_suspended", teams.ErrSuspended.Error())
		case errors.Is(err, teams.ErrSeatRequired):
			writeError(w, r, 409, "seat_required", teams.ErrSeatRequired.Error())
		case errors.Is(err, teams.ErrExpired), errors.Is(err, teams.ErrConflict), errors.Is(err, teams.ErrLimit):
			writeError(w, r, 409, "invitation_unavailable", "Invitation was accepted or cancelled, or the team has no available seat. Refresh the team.")
		case err != nil:
			writeError(w, r, 500, "internal_error", "Resend could not be confirmed. Retry the same operation_id.", err)
		default:
			writeJSON(w, 200, SuccessResponse{Data: item})
		}
	})))

	mux.Handle("GET /v1/team-invitations/{invitation_id}", read(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		item, err := discovery.Invitation(r.Context(), account, r.PathValue("invitation_id"))
		if errors.Is(err, teams.ErrNotFound) {
			writeError(w, r, 404, "not_found_or_forbidden", "Invitation was not found.")
			return
		}
		if err != nil {
			writeError(w, r, 503, "invitations_unavailable", "Invitation could not be loaded.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: item})
	})))
	mux.Handle("POST /v1/team-invitations/{invitation_id}/decline", write(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 4096)
		var request teams.AcceptRequest
		if !decodeStrictJSON(w, r, &request) {
			return
		}
		item, err := discovery.DeclineInvitation(r.Context(), account, r.PathValue("invitation_id"), request)
		switch {
		case errors.Is(err, teams.ErrNotFound), errors.Is(err, teams.ErrForbidden):
			writeError(w, r, 404, "not_found_or_forbidden", "Invitation was not found.")
		case errors.Is(err, teams.ErrConflict):
			writeError(w, r, 409, "invitation_conflict", "Invitation changed; reload it before retrying.")
		case errors.Is(err, teams.ErrInvalid):
			writeError(w, r, 400, "invalid_request", "Provide a valid operation ID.")
		case err != nil:
			writeError(w, r, 503, "invitations_unavailable", "Invitation could not be declined.", err)
		default:
			writeJSON(w, 200, SuccessResponse{Data: item})
		}
	})))

}
