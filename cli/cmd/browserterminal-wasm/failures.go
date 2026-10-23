package main

import (
	"errors"
	"io"
)

type terminalConnectError struct {
	state   string
	code    string
	message string
	details map[string]any
	cause   error
}

func (e *terminalConnectError) Error() string { return e.message }
func (e *terminalConnectError) Unwrap() error { return e.cause }

// Only authored protocol codes cross into browser failure presentation. Remote
// prose and unknown codes may contain private data and are never interpolated.
func browserFileFailure(remoteCode string) (string, string) {
	code := remoteCode
	switch code {
	case "credential_expired", "not_found_or_forbidden", "stale_generation",
		"invalid_path", "invalid_size", "batch_limit", "offset_conflict",
		"state_conflict", "digest_mismatch", "storage_unavailable",
		"resource_limit", "canceled", "delivery_timeout", "recipient_unavailable",
		"capability_required", "capability_disabled", "invalid_request",
		"operation_canceled", "deadline_exceeded", "operation_id_conflict",
		"operation_uncertain", "unavailable":
	default:
		code = "file_failed"
	}
	return code, "The file upload could not finish. Reconnect and retry; no path was pasted."
}

// bandwidthQuotaExhausted is the edge's WebSocket close reason when the workspace's hosted bandwidth is
// used up; the console stops reconnecting on this code and shows the plan limit.
const bandwidthQuotaExhausted = "bandwidth_quota_exhausted"

// closeFailure turns the WebSocket close reason into the terminal's outcome: the plan-limit close is a
// denial (no reconnect loop); any other close is a plain end of stream.
func closeFailure(reason string) error {
	if reason == bandwidthQuotaExhausted {
		return &terminalConnectError{state: "denied", code: bandwidthQuotaExhausted, message: "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth or upgrade under Billing in the Paperboat console, or wait for the next allowance period."}
	}
	return io.EOF
}

// innermostConnectError prefers the most specific outcome: a transport failure wrapping the socket's
// plan-limit denial is reported as the denial.
func innermostConnectError(err *terminalConnectError) *terminalConnectError {
	for err.cause != nil {
		var inner *terminalConnectError
		if !errors.As(err.cause, &inner) {
			break
		}
		err = inner
	}
	return err
}
