package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
)

type environmentMachinePrincipalKey struct{}

type environmentMachinePrincipal struct {
	AccountID   string
	MachineID   string
	OperationID string
}

// environmentEnrollmentMachineOrHuman accepts the existing human/machine
// authentication path or an existing signed machine-control proof. The raw
// body is restored after proof verification so both paths parse identical JSON.
func environmentEnrollmentMachineOrHuman(verifier machineEndpointProofVerifier, human, machine http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Proof")) == "" {
			human.ServeHTTP(w, r)
			return
		}
		if verifier == nil {
			writeError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "Machine identity proof was rejected.")
			return
		}
		maxBody := int64(32 << 10)
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		proof, proofErr := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(r.Header.Get("X-Paperboat-Machine-Proof")))
		scheme, credential, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		if err != nil || int64(len(body)) > maxBody || proofErr != nil || len(proof) == 0 || !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(credential) == "" {
			writeError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "Machine identity proof was rejected.")
			return
		}
		claims, err := verifier.VerifyMachineRequest(r.Context(), strings.TrimSpace(credential), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "machine_identity_invalid", "Machine identity proof was rejected.")
			return
		}
		if claims.UserID == "" || claims.MachineID == "" {
			logHTTPRejection(r, http.StatusUnauthorized, "machine_identity_invalid", controlplane.ErrHelperProof)
			writeError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "Machine identity proof was rejected.")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ctx := context.WithValue(r.Context(), environmentMachinePrincipalKey{}, environmentMachinePrincipal{AccountID: claims.UserID, MachineID: claims.MachineID, OperationID: claims.OperationID})
		machine.ServeHTTP(w, r.WithContext(ctx))
	})
}

type environmentVariableAPI interface {
	PasswordVaultIssuer() string
	GetPasswordVault(context.Context, string) (environment.PasswordVaultHead, error)
	PutPasswordVault(context.Context, string, []byte) (environment.PasswordVaultHead, error)
}

func environmentPasswordVaultGet(service environmentVariableAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		head, err := service.GetPasswordVault(r.Context(), p.User.ID)
		if err != nil {
			if expectedHTTPErrorOnly(err, environment.ErrNotFound) {
				writeErrorDetails(w, r, http.StatusNotFound, "not_found", "ENV credential vault has not been initialized.", map[string]any{"issuer": service.PasswordVaultIssuer()})
				return
			}
			writeEnvironmentError(w, r, err)
			return
		}
		w.Header().Set("ETag", `"`+head.DocumentID+`"`)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: head})
	}
}

func environmentPasswordVaultPut(service environmentVariableAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		limitEnvironmentJSON(w, r, 360<<10)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if !requirePersonalWorkspaceTarget(w, r) {
			return
		}
		var body struct {
			Envelope string `json:"envelope"`
		}
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		raw, err := environment.DecodeCanonicalBase64URL(body.Envelope, environment.MaximumPasswordVaultBytes)
		if err != nil {
			writeEnvironmentError(w, r, err)
			return
		}
		head, err := service.PutPasswordVault(r.Context(), p.User.ID, raw)
		if err != nil {
			writeEnvironmentError(w, r, err)
			return
		}
		w.Header().Set("ETag", `"`+head.DocumentID+`"`)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: head})
	}
}

func limitEnvironmentJSON(w http.ResponseWriter, r *http.Request, max int64) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
}

func writeEnvironmentError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 500, "internal_error", "Environment management operation failed."
	details := map[string]any{}
	switch {
	case expectedHTTPErrorOnly(err, environment.ErrProtocolInvalid, environment.ErrProtocolSignature, environment.ErrInvalidScope, environment.ErrInvalidName, environment.ErrLimitExceeded, environment.ErrObservationInvalid):
		status, code, message = 400, "validation_failed", "Environment management request is invalid."
	case expectedHTTPErrorOnly(err, environment.ErrPrecondition):
		status, code, message = 412, "precondition_failed", "Environment management precondition failed."
	case expectedHTTPErrorOnly(err, environment.ErrNotFound, environment.ErrMachineNotFound):
		status, code, message = 404, "not_found_or_forbidden", "Environment management resource was not found."
	case expectedHTTPErrorOnly(err, environment.ErrMachineEnvironmentUnavailable):
		status, code, message = 422, "machine_environment_unavailable", "Environment management is disabled on this machine."
	case expectedHTTPErrorOnly(err, environment.ErrVersionConflict):
		status, code, message = 409, "version_conflict", "ENV scope changed; fetch it and retry."
		var c *environment.VersionConflictError
		if errors.As(err, &c) {
			details["current_version"] = c.CurrentVersion
		}
	case expectedHTTPErrorOnly(err, environment.ErrPasswordVaultConflict):
		status, code, message = 409, "vault_conflict", "ENV password vault changed; fetch it and retry."
	case expectedHTTPErrorOnly(err, environment.ErrEnvironmentUnconfigured):
		status, code, message = 409, "environment_unconfigured", "ENV has not been configured for this account."
		var unconfigured *environment.EnvironmentUnconfiguredError
		if errors.As(err, &unconfigured) {
			details["machine_id"] = unconfigured.MachineID
			details["installation_generation"] = unconfigured.InstallationGeneration
			details["host_key_generation"] = unconfigured.HostKeyGeneration
			details["host_public"] = unconfigured.HostPublic
			details["account_config_generation"] = uint64(0)
		}
	case expectedHTTPErrorOnly(err, environment.ErrAuthorityConflict):
		status, code, message = 409, "authority_conflict", "ENV authority changed; fetch it and retry."
	case expectedHTTPErrorOnly(err, environment.ErrTransitionInProgress):
		status, code, message = 409, "transition_in_progress", "An ENV authority transition is already in progress."
	case expectedHTTPErrorOnly(err, environment.ErrOperationConflict):
		status, code, message = 409, "operation_conflict", "The ENV operation ID is already bound to different bytes."
	case expectedHTTPErrorOnly(err, environment.ErrKeyAuthorizationRequired):
		status, code, message = 409, "key_authorization_required", "A trusted ENV manager must authorize this key."
	case expectedHTTPErrorOnly(err, environment.ErrAuthorityFork, environment.ErrRootSetChanged):
		status, code, message = 409, "authority_fork", "ENV authority continuity could not be verified."
	}
	writeErrorDetails(w, r, status, code, message, details, err)
}
