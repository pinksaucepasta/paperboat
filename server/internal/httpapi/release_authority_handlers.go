package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/releaseauthority"
)

func releaseAuthorityRequests(service *releaseauthority.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, ok := readInventoryQuery(w, r, nil, false)
		if !ok {
			return
		}
		if query.Q != "" || query.Owner != "" {
			writeError(w, r, 400, "invalid_inventory_query", "This collection supports limit and offset.")
			return
		}
		items, total, err := service.RequestInventory(r.Context(), query.Limit, query.Offset)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to read release authority requests.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"requests": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
	}
}
func releaseAuthorityRequestCreate(service *releaseauthority.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil {
			if expectedHTTPErrorOnly(err, context.Canceled) {
				return
			}
			writeError(w, r, http.StatusServiceUnavailable, "internal_error", "Unable to read the release authority request body.", err)
			return
		}
		if len(raw) == 0 || len(raw) > 4096 {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must match the documented schema.")
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var input releaseauthority.Request
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must match the documented schema.")
			return
		}
		result, err := service.Request(r.Context(), principal.User.ID, r.Header.Get("Idempotency-Key"), input)
		if expectedHTTPErrorOnly(err, releaseauthority.ErrForbidden) {
			writeError(w, r, 403, "forbidden", "Administrator authority is required.")
		} else if expectedHTTPErrorOnly(err, releaseauthority.ErrInvalid) {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Release authority request details are invalid.")
		} else if expectedHTTPErrorOnly(err, releaseauthority.ErrConflict) {
			writeError(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency-Key conflicts with an earlier release authority request.")
		} else if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to create release authority request.", err)
		} else {
			writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
		}
	}
}

func releaseAuthorityBundles(service *releaseauthority.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query, ok := readInventoryQuery(w, r, nil, false)
		if !ok {
			return
		}
		if query.Q != "" || query.Owner != "" {
			writeError(w, r, 400, "invalid_inventory_query", "This collection supports limit and offset.")
			return
		}
		items, total, err := service.BundleInventory(r.Context(), query.Limit, query.Offset)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to read release authority decisions.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"bundles": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
	}
}

func releaseAuthorityBundleImport(service *releaseauthority.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
		if err != nil {
			if expectedHTTPErrorOnly(err, context.Canceled) {
				return
			}
			writeError(w, r, http.StatusServiceUnavailable, "internal_error", "Unable to read the signed release authority bundle.", err)
			return
		}
		if len(raw) == 0 || len(raw) > 64<<10 {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Signed bundle must be a JSON document no larger than 64 KiB.")
			return
		}
		result, err := service.Import(r.Context(), principal.User.ID, r.Header.Get("Idempotency-Key"), raw)
		switch {
		case expectedHTTPErrorOnly(err, releaseauthority.ErrForbidden):
			writeError(w, r, 403, "forbidden", "Administrator authority is required.")
		case expectedHTTPErrorOnly(err, releaseauthority.ErrInvalid):
			writeError(w, r, http.StatusBadRequest, "release_authority_bundle_invalid", "The signed release authority bundle is invalid or expired.")
		case expectedHTTPErrorOnly(err, releaseauthority.ErrSignature):
			writeError(w, r, http.StatusForbidden, "release_authority_signature_invalid", "The bundle does not meet the configured release-authority signing threshold.")
		case expectedHTTPErrorOnly(err, releaseauthority.ErrConflict):
			writeError(w, r, http.StatusConflict, "release_authority_import_conflict", "The bundle policy revision or Idempotency-Key conflicts with an earlier import.")
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to import the signed release authority bundle.", err)
		default:
			writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
		}
	}
}
