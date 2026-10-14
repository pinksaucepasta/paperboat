package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
)

func configConflictRequest(service *controlplane.ConfigConflictService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var input struct {
			Path                      string `json:"path"`
			ConflictRevision          string `json:"conflict_revision"`
			ExpectedRemoteRevision    string `json:"expected_remote_revision"`
			ExpectedAssignmentVersion int64  `json:"expected_assignment_version"`
			Action                    string `json:"action"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		result, err := service.Request(r.Context(), principal.User.ID, r.PathValue("environment_id"), input.ExpectedAssignmentVersion, controlplane.ConfigConflictResolution{
			Path: input.Path, ConflictRevision: input.ConflictRevision,
			ExpectedRemoteRevision: input.ExpectedRemoteRevision, Scope: "path", Action: input.Action,
		})
		if err != nil {
			writeConfigConflictRequestError(w, r, err, "conflict_resolution_invalid", "conflict_resolution_stale", "Conflict resolution could not be requested.")
			return
		}
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: result})
	}
}

func configForceRequest(service *controlplane.ConfigConflictService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var input struct {
			Scope                     string `json:"scope"`
			Path                      string `json:"path"`
			ConflictRevision          string `json:"conflict_revision"`
			ExpectedRemoteRevision    string `json:"expected_remote_revision"`
			ExpectedAssignmentVersion int64  `json:"expected_assignment_version"`
			Action                    string `json:"action"`
			Confirmation              string `json:"confirmation"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		result, err := service.Request(r.Context(), principal.User.ID, r.PathValue("environment_id"), input.ExpectedAssignmentVersion, controlplane.ConfigConflictResolution{
			Path: input.Path, ConflictRevision: input.ConflictRevision,
			ExpectedRemoteRevision: input.ExpectedRemoteRevision, Scope: input.Scope,
			Action: input.Action, Confirmation: input.Confirmation,
		})
		if err != nil {
			writeConfigConflictRequestError(w, r, err, "force_confirmation_required", "force_request_stale", "Force operation could not be requested.")
			return
		}
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: result})
	}
}

func configConflictPending(service *controlplane.ConfigConflictService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, identity, credential, proof, ok := configConflictHelperRequest(w, r)
		if !ok {
			return
		}
		items, err := service.Pending(r.Context(), identity, credential, proof, body, r.Method, r.URL.Path)
		if err != nil {
			if errors.Is(err, controlplane.ErrConfigConflictResolutionInvalid) {
				logHTTPRejection(r, http.StatusUnauthorized, "conflict_resolution_invalid", err)
				writeError(w, r, http.StatusUnauthorized, "conflict_resolution_invalid", "Conflict resolution authorization is invalid.")
			} else {
				writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "Pending configuration changes are temporarily unavailable.", err)
			}
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": items}})
	}
}

func configConflictAcknowledge(service *controlplane.ConfigConflictService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, identity, credential, proof, ok := configConflictHelperRequest(w, r)
		if !ok {
			return
		}
		var input struct {
			ID             string `json:"id"`
			LandedRevision string `json:"landed_revision"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
			writeError(w, r, http.StatusBadRequest, "conflict_resolution_invalid", "Conflict resolution acknowledgement is invalid.")
			return
		}
		if err := service.Acknowledge(r.Context(), identity, credential, proof, body, r.Method, r.URL.Path, input.ID, input.LandedRevision); err != nil {
			switch {
			case errors.Is(err, controlplane.ErrConfigConflictResolutionStale):
				writeError(w, r, http.StatusConflict, "conflict_resolution_invalid", "Conflict resolution acknowledgement was rejected.")
			case errors.Is(err, controlplane.ErrConfigConflictResolutionInvalid):
				logHTTPRejection(r, http.StatusUnauthorized, "conflict_resolution_invalid", err)
				writeError(w, r, http.StatusUnauthorized, "conflict_resolution_invalid", "Conflict resolution acknowledgement was rejected.")
			default:
				writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "Configuration change acknowledgement is temporarily unavailable.", err)
			}
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]bool{"applied": true}})
	}
}

func writeConfigConflictRequestError(w http.ResponseWriter, r *http.Request, err error, invalidCode, staleCode, unavailableMessage string) {
	status, code := http.StatusBadRequest, invalidCode
	switch {
	case errors.Is(err, controlplane.ErrConfigConflictResolutionMode):
		status, code = http.StatusConflict, "mode_forbids_publication"
	case errors.Is(err, controlplane.ErrConfigConflictResolutionStale):
		status, code = http.StatusConflict, staleCode
	case errors.Is(err, controlplane.ErrAssignmentForbidden):
		status, code = http.StatusNotFound, "not_found_or_forbidden"
	case errors.Is(err, controlplane.ErrConfigConflictResolutionInvalid):
		// Invalid input and stale/mode/authority outcomes are expected rejections.
	default:
		writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", unavailableMessage, err)
		return
	}
	writeError(w, r, status, code, unavailableMessage)
}

func configConflictHelperRequest(w http.ResponseWriter, r *http.Request) ([]byte, string, string, []byte, bool) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	proof, proofErr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
	identity := strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Identity"))
	credential, credentialOK := bearerToken(r)
	if err != nil || len(body) >= 64<<10 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
		proofErr != nil || identity == "" || !credentialOK {
		writeError(w, r, http.StatusUnauthorized, "conflict_resolution_invalid", "Conflict resolution authorization is invalid.")
		return nil, "", "", nil, false
	}
	return body, identity, credential, proof, true
}
