package localdaemon

import (
	"context"
	"errors"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type inventorySourceError struct {
	stage string
	cause error
}

func (e *inventorySourceError) Error() string { return "machine inventory failed at " + e.stage }
func (e *inventorySourceError) Unwrap() error { return e.cause }
func inventorySourceFailure(stage string, err error) error {
	var existing *inventorySourceError
	if errors.As(err, &existing) {
		return err
	}
	return &inventorySourceError{stage: stage, cause: err}
}

// Diagnostics contain only locally selected stages/categories and HTTP status.
// API messages, codes, URLs, credentials and response bodies are never recorded.
func inventoryRefreshDiagnostic(err error) (string, map[string]string) {
	fields := map[string]string{"outcome": "ready"}
	if err == nil {
		return "info", fields
	}
	fields["outcome"] = "degraded"
	fields["phase"] = "inventory"
	var source *inventorySourceError
	if errors.As(err, &source) {
		fields["phase"] = source.stage
	}
	reason := "source_error"
	fault := errorreport.ProjectFault(context.Background(), "paperboatd", "machine_control", "reconciliation", "control_request_failed", err)
	switch {
	case fault.Outcome == "canceled":
		reason = "canceled"
	case fault.Cause == "deadline_exceeded":
		reason = "deadline"
	case inventoryAuthenticationRequired(err):
		reason = "unauthenticated"
	case fault.HTTPStatus >= 400 && fault.HTTPStatus <= 599 && fault.Errno == 0 && fault.Cause != "internal" && fault.Cause != "network_timeout":
		reason = "http_" + strconv.Itoa(fault.HTTPStatus)
	case fault.Cause != "internal":
		reason = fault.Cause
	}
	fields["reason"] = reason
	return "warning", fields
}

func (e *inventorySourceError) DiagnosticStage() string {
	switch e.stage {
	case "credential", "peer_approval", "peer_signer", "peer_root", "peer_approve":
		return "peer_authority"
	case "configuration", "machine_alias", "completion_projection":
		return "reconciliation"
	default:
		return "control_request"
	}
}
func (*inventorySourceError) DiagnosticCode() string { return "control_request_failed" }
