package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

const maxMachineControlRequest = 16 << 10

type machineControlInitialIssuer interface {
	IssueInitialMachineControl(context.Context, string, string, string) (usermachines.MachineControlCredential, error)
}

type helperRequestVerifier interface {
	VerifyHelperRequest(context.Context, string, []byte, string, string, []byte) (controlplane.HelperProofClaims, error)
}

func machineControlIssue(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		body, operationID, proof, ok := machineControlRequest(w, r)
		if !ok {
			return
		}
		result, err := service.IssueMachineControl(r.Context(), principal.User.ID, r.PathValue("machine_id"), proof, body, r.Method, r.URL.Path)
		if err != nil || operationID == "" {
			if err != nil && !expectedHTTPErrorOnly(err, usermachines.ErrMachineControlInvalid) {
				writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Machine authorization is temporarily unavailable.", err)
			} else {
				logHTTPRejection(r, http.StatusUnauthorized, "machine_control_proof_invalid", err)
				writeError(w, r, http.StatusUnauthorized, "machine_control_proof_invalid", "Machine identity proof was rejected.")
			}
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
	}
}

func machineControlRenew(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _, proof, ok := machineControlRequest(w, r)
		if !ok {
			return
		}
		scheme, credential, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(credential) == "" {
			writeError(w, r, http.StatusUnauthorized, "machine_control_credential_invalid", "Machine control credential is required.")
			return
		}
		result, err := service.RenewMachineControl(r.Context(), strings.TrimSpace(credential), proof, body, r.Method, r.URL.Path)
		if err != nil {
			var rejection usermachines.ControlRejection
			if usermachines.ControlRejectionReason(err) == usermachines.ProofClockSkewReason {
				logHTTPRejection(r, http.StatusBadRequest, "clock_skew", err)
				writeClockSkewAnswer(w, r)
			} else if errors.As(err, &rejection) || expectedHTTPErrorOnly(err, usermachines.ErrMachineControlInvalid) {
				// A fixed reason code (no credential or request content) names the check that refused it.
				serverdiagnostics.LoggerWithRequest(r.Context(), slog.Default()).WarnContext(r.Context(), "machine control renewal rejected", "reason", usermachines.ControlRejectionReason(err))
				logHTTPRejection(r, http.StatusUnauthorized, "machine_control_credential_invalid", err)
				writeError(w, r, http.StatusUnauthorized, "machine_control_credential_invalid", "Machine control credential or proof was rejected.")
			} else {
				writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Machine authorization is temporarily unavailable.", err)
			}
			return
		}
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
	}
}

// machineControlInitial exchanges the short-lived helper identity created by
// enrollment for a machine-control credential. It is intentionally separate
// from the user-session endpoint: bootstrap must be restartable before the
// local CLI session is available, while the helper identity is already bound
// to the enrolled machine key and environment.
func machineControlInitial(service machineControlInitialIssuer, identities helperRequestVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, operationID, proof, ok := machineControlRequest(w, r)
		if !ok {
			return
		}
		scheme, credential, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(credential) == "" {
			writeError(w, r, http.StatusUnauthorized, "machine_control_bootstrap_invalid", "Machine bootstrap identity was rejected.")
			return
		}
		claims, err := identities.VerifyHelperRequest(r.Context(), strings.TrimSpace(credential), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "machine_control_bootstrap_invalid", "Machine bootstrap identity was rejected.")
			return
		}
		result, err := service.IssueInitialMachineControl(r.Context(), claims.MachineID, claims.EnvironmentID, operationID)
		if err != nil {
			if expectedHTTPErrorOnly(err, usermachines.ErrMachineControlInvalid) {
				logHTTPRejection(r, http.StatusUnauthorized, "machine_control_bootstrap_invalid", err)
				writeError(w, r, http.StatusUnauthorized, "machine_control_bootstrap_invalid", "Machine bootstrap identity was rejected.")
			} else {
				writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Machine authorization is temporarily unavailable.", err)
			}
			return
		}
		noStore(w)
		writeJSON(w, http.StatusCreated, SuccessResponse{Data: result})
	}
}

func machineControlRequest(w http.ResponseWriter, r *http.Request) ([]byte, string, []byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMachineControlRequest+1))
	if err != nil || len(body) > maxMachineControlRequest {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body is invalid.")
		return nil, "", nil, false
	}
	var request struct {
		OperationID string `json:"operation_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || len(request.OperationID) < 8 || len(request.OperationID) > 128 {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "A valid operation_id is required.")
		return nil, "", nil, false
	}
	proof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
	if err != nil || len(proof) == 0 {
		writeError(w, r, http.StatusUnauthorized, "machine_control_proof_invalid", "Machine identity proof is required.")
		return nil, "", nil, false
	}
	return body, request.OperationID, proof, true
}
