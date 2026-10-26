package main

import (
	"errors"
	"io"

	"github.com/pinksaucepasta/paperboat/shared/terminalend"
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

// closeFailure turns the WebSocket close reason into the terminal's outcome: the plan-limit close is a
// denial (no reconnect loop); any other close is a plain end of stream.
// closeFailure turns a WebSocket close into the terminal's outcome. A close without a code (1006: the
// network dropped) reconnects. Otherwise the reason is a typed code (shared/terminalend), or the close
// number names it when the reason is empty; the state event carries the typed code, which the console
// branches on and words, plus the raw close number and whether the close was clean.
func closeFailure(code int, reason string, clean bool) error {
	if code == 0 || code == 1006 || code == 1000 && reason == "" {
		return io.EOF
	}
	ended := terminalend.Parse(reason)
	if reason == "" {
		ended = terminalend.ForCloseCode(code)
	}
	state := "transport-error"
	switch ended {
	case terminalend.Ended:
		state = "closed"
	case terminalend.NoAccess, terminalend.Revoked, terminalend.Limit, terminalend.BandwidthQuotaExhausted:
		state = "denied"
	}
	message := ""
	if ended == terminalend.BandwidthQuotaExhausted {
		message = "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth or upgrade under Billing in the Paperboat console, or wait for the next allowance period."
	}
	return &terminalConnectError{state: state, code: string(ended), message: message, details: map[string]any{"close": code, "clean": clean}}
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
