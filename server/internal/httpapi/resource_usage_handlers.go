package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/resourceusage"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func resourceUsageGet(service *resourceusage.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		scope, present, err := requestWorkspace(r)
		if err != nil {
			writeWorkspaceError(w, r, err)
			return
		}
		if !present {
			scope = workspace.Scope{ID: "personal"}
		}
		values := r.URL.Query()
		allowed := map[string]bool{"workspace": true, "start": true, "end": true, "resource_id": true, "resource_kind": true, "resource_route_id": true, "machine_id": true, "mode": true, "hosting": true, "limit": true, "offset": true, "path_limit": true, "path_offset": true}
		for key, entries := range values {
			if !allowed[key] || len(entries) != 1 {
				writeError(w, r, 400, "invalid_usage_query", "Usage filters must be supported and occur once.")
				return
			}
		}
		number := func(key string, fallback, min, max int) (int, bool) {
			if !values.Has(key) {
				return fallback, true
			}
			n, err := strconv.Atoi(values.Get(key))
			return n, err == nil && n >= min && n <= max
		}
		limit, lok := number("limit", 50, 1, resourceusage.MaxResources)
		offset, ook := number("offset", 0, 0, 2147483647)
		pathLimit, plok := number("path_limit", resourceusage.MaxPaths, 1, resourceusage.MaxPaths)
		pathOffset, pook := number("path_offset", 0, 0, 2147483647)
		if !lok || !ook || !plok || !pook {
			writeError(w, r, 400, "invalid_usage_query", "Choose valid usage page limits and offsets.")
			return
		}
		var route, machine *string
		if values.Has("resource_route_id") {
			v := values.Get("resource_route_id")
			route = &v
		}
		if values.Has("machine_id") {
			v := values.Get("machine_id")
			machine = &v
		}

		start, e1 := time.Parse(time.RFC3339, values.Get("start"))
		end, e2 := time.Parse(time.RFC3339, values.Get("end"))
		if e1 != nil || e2 != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_usage_query", "Choose RFC3339 start and end timestamps within a 90-day window.")
			return
		}
		result, err := service.Get(r.Context(), p.User.ID, scope, resourceusage.Query{Start: start, End: end, ResourceID: values.Get("resource_id"), Mode: values.Get("mode"), Hosting: values.Get("hosting"), ResourceKind: values.Get("resource_kind"), RouteID: route, MachineID: machine, Limit: limit, Offset: offset, PathLimit: pathLimit, PathOffset: pathOffset})
		switch {
		case errors.Is(err, resourceusage.ErrInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_usage_query", "Choose valid usage filters and a time window of at most 90 days.")
		case errors.Is(err, workspace.ErrForbidden) || errors.Is(err, workspace.ErrInvalid):
			writeWorkspaceError(w, r, err)
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "usage_unavailable", "Bandwidth history is unavailable.", err)
		default:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, SuccessResponse{Data: result})
		}
	}
}

func resourceUsageNative(identities machineRequestVerifier, service *resourceusage.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil {
			if expectedHTTPErrorOnly(err, context.Canceled) {
				return
			}
			writeError(w, r, 503, "usage_unavailable", "Bandwidth reporting is temporarily unavailable.", err)
			return
		}
		if len(body) > 65536 {
			writeError(w, r, 400, "invalid_usage_report", "Bandwidth report is invalid.")
			return
		}
		var batch resourceusage.NativeBatch
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&batch) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, r, 400, "invalid_usage_report", "Bandwidth report is invalid.")
			return
		}
		proof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || len(proof) == 0 || identities == nil {
			writeError(w, r, 401, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		identity, err := identities.VerifyMachineRequest(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		receipt, err := service.RecordNative(r.Context(), identity.MachineID, identity.InstallationGeneration, batch)
		switch {
		case errors.Is(err, resourceusage.ErrInvalid):
			writeError(w, r, 400, "invalid_usage_report", "Bandwidth report is invalid or outside the delivery window.")
		case err != nil:
			writeError(w, r, 503, "usage_unavailable", "Bandwidth reporting is temporarily unavailable.", err)
		default:
			writeJSON(w, 200, SuccessResponse{Data: receipt})
		}
	}
}
