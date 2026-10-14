package main

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
