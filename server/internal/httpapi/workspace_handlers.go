package httpapi

import (
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func workspacesList(service *workspace.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		items, err := service.List(r.Context(), p.User.ID)
		if err != nil {
			writeWorkspaceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": items}})
	}
}
