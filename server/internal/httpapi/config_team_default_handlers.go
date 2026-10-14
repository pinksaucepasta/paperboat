package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
)

func configTeamDefault(service *controlplane.ConfigAssignmentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		teamID := r.PathValue("team_id")
		if !requireTeamWorkspaceTarget(w, r, teamID) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			item, err := service.TeamDefault(r.Context(), p.User.ID, teamID)
			if err != nil {
				configDefaultError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: item})
		case http.MethodPut:
			var in struct {
				RepositoryID    string `json:"repository_id"`
				ExpectedVersion int64  `json:"expected_version"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			item, err := service.SetTeamDefault(r.Context(), p.User.ID, teamID, in.RepositoryID, in.ExpectedVersion)
			if err != nil {
				configDefaultError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: item})
		case http.MethodDelete:
			version, _ := strconv.ParseInt(r.URL.Query().Get("expected_version"), 10, 64)
			if err := service.DeleteTeamDefault(r.Context(), p.User.ID, teamID, version); err != nil {
				configDefaultError(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func configTeamDefaultAdoption(service *controlplane.ConfigAssignmentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		switch r.Method {
		case http.MethodGet:
			item, err := service.TeamDefaultAdoption(r.Context(), p.User.ID)
			if err != nil {
				configDefaultError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: item})
		case http.MethodPut:
			var in struct {
				TeamID         string `json:"team_id"`
				DefaultVersion int64  `json:"default_version"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			if !requireTeamWorkspaceTarget(w, r, in.TeamID) {
				return
			}
			item, err := service.AdoptTeamDefault(r.Context(), p.User.ID, in.TeamID, in.DefaultVersion)
			if err != nil {
				configDefaultError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: item})
		case http.MethodDelete:
			if err := service.UnadoptTeamDefault(r.Context(), p.User.ID); err != nil {
				configDefaultError(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func configDefaultError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, controlplane.ErrAssignmentConflict) {
		writeError(w, r, 409, "version_conflict", "Configuration default changed; refresh and retry.")
		return
	}
	if errors.Is(err, controlplane.ErrAssignmentForbidden) || errors.Is(err, controlplane.ErrAssignmentInvalid) {
		writeError(w, r, 404, "not_found_or_forbidden", "Configuration default is unavailable.")
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "configuration_default_unavailable", "Configuration default is temporarily unavailable.", err)
}
