package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
)

type ErrorResponse struct {
	Error APIError `json:"error"`
}

type APIError struct {
	SupportReference string         `json:"support_reference,omitempty"`
	Schema           string         `json:"schema,omitempty"`
	Kind             string         `json:"kind,omitempty"`
	Code             string         `json:"code"`
	Component        string         `json:"component,omitempty"`
	Message          string         `json:"message"`
	Outcome          string         `json:"outcome,omitempty"`
	Retryable        *bool          `json:"retryable,omitempty"`
	RetryAt          *time.Time     `json:"retry_at,omitempty"`
	RepairAction     string         `json:"repair_action,omitempty"`
	RequestID        string         `json:"request_id"`
	CorrelationID    string         `json:"correlation_id,omitempty"`
	Details          map[string]any `json:"details,omitempty"`
}

type SuccessResponse struct {
	Data any `json:"data"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, causes ...error) {
	writeErrorDetails(w, r, status, code, message, map[string]any{}, causes...)
}

// writeMachineProofFailure keeps rejected proofs distinct from verifier
// outages. Proof verifiers use ErrHelperProof (or the attachment contract's
// ErrUnauthorized) only for invalid credentials; database and operational
// failures retain their original cause and produce an actionable 503.
func writeMachineProofFailure(w http.ResponseWriter, r *http.Request, err error, rejectedCode, rejectedMessage string) {
	if writeClockSkew(w, r, err) {
		return
	}
	if machineProofRejected(err) {
		logHTTPRejection(r, http.StatusUnauthorized, rejectedCode, err)
		writeError(w, r, http.StatusUnauthorized, rejectedCode, rejectedMessage)
		return
	}
	writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Machine authorization is temporarily unavailable.", err)
}

func machineProofRejected(err error) bool {
	return pureMachineProofDenials(err)
}

const (
	maxMachineProofErrorDepth = 8
	maxMachineProofErrorNodes = 16
)

// A verifier error is an expected rejection only when its complete, bounded
// unwrap tree consists of the known credential-denial sentinels. A joined
// database or other unknown failure must remain operationally visible.
func pureMachineProofDenials(err error) (rejected bool) {
	defer func() {
		if recover() != nil {
			rejected = false
		}
	}()
	if err == nil {
		return false
	}
	nodes, matched := 0, false
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= maxMachineProofErrorDepth || nodes >= maxMachineProofErrorNodes {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			return child != nil && visit(child, depth+1)
		}
		if !typ.Comparable() {
			return false
		}
		if typ == reflect.TypeOf(controlplane.ErrHelperProof) && current == controlplane.ErrHelperProof || typ == reflect.TypeOf(previewattachment.ErrUnauthorized) && current == previewattachment.ErrUnauthorized {
			matched = true
			return true
		}
		return false
	}
	rejected = visit(err, 0) && matched
	return rejected
}

func writePreviewMachineProofFailure(w http.ResponseWriter, r *http.Request, err error) {
	if writeClockSkew(w, r, err) {
		return
	}
	if machineProofRejected(err) {
		logHTTPRejection(r, http.StatusUnauthorized, "machine_identity_invalid", err)
		writePreviewTunnelError(w, r, http.StatusUnauthorized, "machine_identity_invalid", "The signed host identity could not be verified.", "unchanged", false, "run_on_authorized_host")
		return
	}
	writePreviewTunnelError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "Machine authorization is temporarily unavailable.", "unchanged", true, "retry", err)
}

func writeErrorDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any, causes ...error) {
	// An API token's scope refusal can come out of any machine authorization; every handler
	// reports it the same way.
	for _, cause := range causes {
		if errors.Is(cause, auth.ErrOutsideTokenScope) {
			status, code, message, causes = http.StatusForbidden, "outside_token_scope", "This API token's scope does not cover this machine action.", nil
			break
		}
	}
	captureSupportFailure(r, status, code, sentry.NewStacktrace(), causes...)
	if status >= 400 && status < 500 {
		// A refusal's code is otherwise only in the response body; the request log
		// line has the status alone, which made account-wide 409s untraceable.
		slog.InfoContext(r.Context(), "http_refusal", "status", status, "code", code, "path", r.URL.Path, "support_reference", supportReference(r.Context()))
	}
	writeJSON(w, status, ErrorResponse{Error: APIError{
		SupportReference: supportReference(r.Context()),
		Code:             code,
		Message:          message,
		RequestID:        requestIDFromContext(r.Context()),
		CorrelationID:    serverdiagnostics.CorrelationID(r.Context()),
		Details:          details,
	}})
}

// writeClockSkew answers a correctly signed proof made with a clock outside the window. It is not a
// 401: the machine is still signed in; it must correct its clock (Date and details.server_time).
func writeClockSkew(w http.ResponseWriter, r *http.Request, err error) bool {
	if !machineProofRejected(err) || !errors.Is(err, controlplane.ErrClockSkew) {
		return false
	}
	logHTTPRejection(r, http.StatusBadRequest, "clock_skew", err)
	writeErrorDetails(w, r, http.StatusBadRequest, "clock_skew", "This machine's clock is off by more than a minute. Correct the system time; Paperboat retries with the server's time.",
		map[string]any{"server_time": time.Now().UTC().Format(time.RFC3339), "allowed_skew_seconds": int(controlplane.ProofClockWindow / time.Second)})
	return true
}
