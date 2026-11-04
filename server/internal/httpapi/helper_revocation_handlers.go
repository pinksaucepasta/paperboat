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

func helperRevocations(edge *controlplane.EdgeService, identities *controlplane.EnrollmentService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		maxBody := int64(controlplane.MaxLiveOperationJTIs * 160)
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || int64(len(body)) > maxBody {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must match the documented schema.")
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var input struct {
			ActiveJTIs []string `json:"active_jtis"`
		}
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must match the documented schema.")
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		proof, proofErr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || proofErr != nil || len(proof) == 0 {
			writeError(w, r, http.StatusUnauthorized, "credential_invalid", "Helper identity is invalid.")
			return
		}
		identity, err := identities.VerifyHelperRequest(r.Context(), parts[1], proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "credential_invalid", "Helper identity is invalid.")
			return
		}
		jtis, err := edge.OperationRevocations(r.Context(), identity.EnvironmentID, input.ActiveJTIs)
		if errors.Is(err, controlplane.ErrLiveOperationJTIsInvalid) {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "active_jtis must contain the bounded list of live operation credential IDs.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "Revocation state is temporarily unavailable.", err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			JTIs []string `json:"jtis"`
		}{JTIs: jtis})
	}
}
