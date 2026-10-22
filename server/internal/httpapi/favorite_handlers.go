package httpapi

import (
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/favorites"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func favoritesList(service *favorites.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		items, err := service.List(personalWorkspaceContext(r.Context()), p.User.ID)
		if err != nil {
			writeFavoriteListError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: items})
	}
}

func favoriteSet(service *favorites.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		body, ok := decodeFavoriteSetRequest(w, r)
		if !ok {
			return
		}
		body.Kind, body.ResourceID = strings.TrimSpace(body.Kind), strings.TrimSpace(body.ResourceID)
		ctx := personalWorkspaceContext(r.Context())
		if err := service.Set(ctx, p.User.ID, body.Kind, body.ResourceID, *body.Favorite); err != nil {
			writeFavoriteSetError(w, r, err)
			return
		}
		items, err := service.List(ctx, p.User.ID)
		if err != nil {
			writeFavoriteListError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: items})
	}
}

type favoriteSetRequest struct {
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id"`
	Favorite   *bool  `json:"favorite"`
}

func decodeFavoriteSetRequest(w http.ResponseWriter, r *http.Request) (favoriteSetRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var body favoriteSetRequest
	if !decodeStrictJSON(w, r, &body) {
		return favoriteSetRequest{}, false
	}
	if body.Favorite == nil {
		writeError(w, r, http.StatusBadRequest, "invalid_favorite", "Favorite must be explicitly set to true or false.")
		return favoriteSetRequest{}, false
	}
	return body, true
}

func writeFavoriteListError(w http.ResponseWriter, r *http.Request, err error) {
	if writeFavoriteWorkspaceError(w, r, err) {
		return
	}
	writeError(w, r, http.StatusInternalServerError, "internal_error", "Favorites could not be loaded.", err)
}

func writeFavoriteSetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, favorites.ErrLimit):
		writeError(w, r, http.StatusConflict, "favorite_limit_reached", "You can favorite up to five items.")
	case expectedHTTPErrorOnly(err, favorites.ErrResourceNotFound):
		writeError(w, r, http.StatusNotFound, "favorite_resource_not_found", "The selected resource was not found.")
	case writeFavoriteWorkspaceError(w, r, err):
	case expectedHTTPErrorOnly(err, favorites.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_favorite", "Favorite kind and resource ID are required.")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Favorite could not be updated.", err)
	}
}

func writeFavoriteWorkspaceError(w http.ResponseWriter, r *http.Request, err error) bool {
	if expectedHTTPErrorOnly(err, workspace.ErrInvalid) || expectedHTTPErrorOnly(err, workspace.ErrForbidden, workspace.ErrSSORequired) {
		writeWorkspaceError(w, r, err)
		return true
	}
	return false
}
