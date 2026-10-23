package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

func configRepositoryAccessIssue(service *controlplane.ConfigRepositoryAccessService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(body) > 4096 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, r, http.StatusBadRequest, "access_invalid", "Repository access request is invalid.")
			return
		}
		var input struct {
			OperationID string `json:"operation_id"`
			Direction   string `json:"direction"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
			writeError(w, r, http.StatusBadRequest, "access_invalid", "Repository access request is invalid.")
			return
		}
		proof, proofErr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		identity := strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Identity"))
		credential, credentialOK := bearerToken(r)
		if proofErr != nil || identity == "" || !credentialOK {
			writeError(w, r, http.StatusUnauthorized, "access_invalid", "Repository access authorization is invalid.")
			return
		}
		access, err := service.Issue(r.Context(), identity, credential, proof, body, r.Method, r.URL.Path, strings.TrimSpace(input.OperationID), strings.TrimSpace(input.Direction))
		switch {
		case errors.Is(err, controlplane.ErrConfigRepositoryAccessReplay):
			writeError(w, r, http.StatusConflict, "operation_conflict", "Repository access operation conflicts with an earlier request.")
			return
		case errors.Is(err, controlplane.ErrConfigRepositoryAccessUncertain):
			writeError(w, r, http.StatusServiceUnavailable, "access_uncertain", "Repository access issuance outcome is uncertain.", err)
			return
		case err != nil:
			if errors.Is(err, controlplane.ErrConfigRepositoryAccessInvalid) {
				logHTTPRejection(r, http.StatusUnauthorized, "access_invalid", err)
				writeError(w, r, http.StatusUnauthorized, "access_invalid", "Repository access authorization is invalid.")
			} else {
				writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "Repository access is temporarily unavailable.", err)
			}
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: access})
	}
}

func configRepositoryReadAccess(service *controlplane.ConfigRepositoryAccessService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var in struct {
			URL string `json:"url"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		access, err := service.IssueReadAccess(r.Context(), p.User.ID, r.PathValue("repository_id"), in.URL)
		if err != nil {
			writeConfigRepositoryReadAccessError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: access})
	}
}

func writeConfigRepositoryReadAccessError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, controlplane.ErrConfigRepositoryAccessInvalid) {
		writeError(w, r, http.StatusBadRequest, "repository_read_access_unavailable", "Read access is unavailable; verify repository ownership and current authorization.")
		return
	}
	if errors.Is(err, controlplane.ErrConfigRepositoryAccessUncertain) {
		writeError(w, r, http.StatusServiceUnavailable, "access_uncertain", "Provider read access could not be confirmed. Keep the support reference and retry after service recovery.", err)
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "configuration_unavailable", "Repository read authorization could not be revalidated. Keep the support reference and retry after service recovery.", err)
}
