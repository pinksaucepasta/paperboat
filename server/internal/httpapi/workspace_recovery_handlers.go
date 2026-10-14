package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

type workspaceRecoveryAPI interface {
	RetireNotApplied(context.Context, string, string, string, string) error
	ReconcileCheckout(context.Context, string, string, string, string, string) error
	SupersedePortal(context.Context, string, string, string, string) error
}

func workspaceRecoveryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, billing.ErrWorkspaceForbidden):
		writeError(w, r, 403, "forbidden", "Your current account authority does not permit payment recovery.")
	case errors.Is(err, billing.ErrWorkspaceRecoveryInvalid), errors.Is(err, billing.ErrBillingRecoveryInvalid):
		writeError(w, r, 400, "validation_failed", "Payment recovery request is invalid.")
	case errors.Is(err, billing.ErrBillingRecoveryConflict):
		writeError(w, r, 409, "idempotency_key_conflict", "This recovery key belongs to a different request.")
	case errors.Is(err, billing.ErrWorkspaceRecoveryTooEarly):
		writeError(w, r, 409, "operation_retry_window_active", "The payment operation is still within its safe retry window. Retry the original operation.")
	case errors.Is(err, billing.ErrBillingOperationNotUncertain):
		writeError(w, r, 409, "operation_not_recoverable", "The payment operation is no longer uncertain.")
	default:
		writeError(w, r, 503, "recovery_unavailable", "Payment recovery could not be confirmed. Retry with the same recovery key.", err)
	}
}

func recoveryIdentity(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	p, ok := principalFromContext(r.Context())
	if !ok {
		writeError(w, r, 401, "unauthenticated", "Authentication is required.")
		return "", "", false
	}
	if p.User.Role != auth.RoleAdmin && p.User.Role != auth.RoleBilling {
		writeError(w, r, 403, "forbidden", "Only an administrator can recover an uncertain payment operation.")
		return "", "", false
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, r, 400, "idempotency_key_required", "Idempotency-Key header is required.")
		return "", "", false
	}
	return p.User.ID, key, true
}

func adminReconcileWorkspaceCheckout(service workspaceRecoveryAPI) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, key, ok := recoveryIdentity(w, r)
		if !ok {
			return
		}
		var in struct {
			ProviderSessionID string `json:"provider_session_id"`
			EvidenceReference string `json:"evidence_reference"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		err := service.ReconcileCheckout(r.Context(), actor, key, r.PathValue("operation_id"), in.ProviderSessionID, in.EvidenceReference)
		if err != nil {
			workspaceRecoveryError(w, r, err)
			return
		}
		w.WriteHeader(204)
	})
}

func adminSupersedeWorkspacePortal(service workspaceRecoveryAPI) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, key, ok := recoveryIdentity(w, r)
		if !ok {
			return
		}
		var in struct {
			EvidenceReference string `json:"evidence_reference"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		err := service.SupersedePortal(r.Context(), actor, key, r.PathValue("operation_id"), in.EvidenceReference)
		if err != nil {
			workspaceRecoveryError(w, r, err)
			return
		}
		w.WriteHeader(204)
	})
}

func adminRetireWorkspaceBillingOperation(service workspaceRecoveryAPI) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if p.User.Role != auth.RoleAdmin && p.User.Role != auth.RoleBilling {
			writeError(w, r, http.StatusForbidden, "forbidden", "Only an administrator can retire an uncertain payment operation.")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required.")
			return
		}
		var input struct {
			Outcome           string `json:"outcome"`
			EvidenceReference string `json:"evidence_reference"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		if input.Outcome != "not_applied" {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Retirement requires provider evidence that the operation was not applied.")
			return
		}
		err := service.RetireNotApplied(r.Context(), p.User.ID, key, r.PathValue("operation_id"), input.EvidenceReference)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, billing.ErrWorkspaceForbidden):
			writeError(w, r, http.StatusForbidden, "forbidden", "Your current account authority does not permit payment recovery.")
		case errors.Is(err, billing.ErrWorkspaceRecoveryInvalid), errors.Is(err, billing.ErrBillingRecoveryInvalid):
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Payment recovery request is invalid.")
		case errors.Is(err, billing.ErrBillingRecoveryConflict):
			writeError(w, r, http.StatusConflict, "idempotency_key_conflict", "This recovery key belongs to a different request.")
		case errors.Is(err, billing.ErrWorkspaceRecoveryTooEarly):
			writeError(w, r, http.StatusConflict, "operation_retry_window_active", "The payment operation is still within its safe retry window. Retry the original operation.")
		case errors.Is(err, billing.ErrBillingOperationNotUncertain):
			writeError(w, r, http.StatusConflict, "operation_not_recoverable", "The payment operation is no longer uncertain.")
	default:
		writeError(w, r, http.StatusServiceUnavailable, "recovery_unavailable", "Payment recovery could not be confirmed. Retry with the same recovery key.", err)
		}
	})
}
