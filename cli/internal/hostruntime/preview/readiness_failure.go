package preview

import (
	"context"
	"net/http"
)

// readinessFailure carries only a finite validation step into dispatch cleanup.
// The original cause remains available for errors.Is, never formatted into diagnostics.
type readinessFailure struct {
	method, stage string
	status        int
	class, cause  error
}

func (e *readinessFailure) Error() string   { return "preview " + e.code() + " failed" }
func (e *readinessFailure) Unwrap() []error { return []error{e.class, e.cause} }

func (e *readinessFailure) code() string {
	prefix := "readiness"
	switch e.method {
	case http.MethodPost:
		prefix = "readiness_post"
	case http.MethodGet:
		prefix = "readiness_get"
	}
	switch e.stage {
	case "request", "identity", "proof", "network", "response_body", "http_status", "json_decode", "etag_invalid", "identity_changed", "stale_generation", "lease_state_invalid", "publish_validation", "owner_update", "owner_projection", "owner_validate", "owner_accept_renewal", "owner_renewal_rekey", "owner_mark_ready", "owner_ready_rekey", "carrier_projection", "carrier_identity", "carrier_generation":
		return prefix + "_" + e.stage
	}
	return prefix + "_validation"
}

func readinessFailed(method, stage string, status int, class, cause error) error {
	return &readinessFailure{method: method, stage: stage, status: status, class: class, cause: cause}
}

func readinessFailureDiagnostic(err error) (string, int) {
	nodes, valid := machineAttachmentDialErrorTree(err)
	if !valid {
		return "", 0
	}
	for _, node := range nodes {
		if failure, ok := node.err.(*readinessFailure); ok {
			status := failure.status
			if status < 100 || status > 599 {
				status = 0
			}
			return failure.code(), status
		}
	}
	return "", 0
}

func readinessProjectionFailure(method string, status int, expected, observed Lease, generation int64) error {
	if !sameLeaseIdentity(expected, observed) {
		return readinessFailed(method, "identity_changed", status, ErrDispatchInvalid, nil)
	}
	if observed.Generation < generation || method == http.MethodPost && observed.Generation == generation {
		return readinessFailed(method, "stale_generation", status, ErrDispatchInvalid, nil)
	}
	if !validActiveLeaseState(observed.State) {
		return readinessFailed(method, "lease_state_invalid", status, ErrDispatchInvalid, nil)
	}
	return nil
}

// Preserve the owner step without masking a more precise wire classification.
func readinessOwnerFailed(stage string, err error) error {
	if err == nil || err == context.Canceled {
		return err
	}
	if reason, _ := readinessFailureDiagnostic(err); reason != "" {
		return err
	}
	return readinessFailed("", stage, 0, nil, err)
}

type readinessRetryError struct{ cause error }

func (e *readinessRetryError) Error() string {
	return ErrDispatchUnavailable.Error() + ": readiness raced repeated lease renewals"
}
func (e *readinessRetryError) Unwrap() []error { return []error{ErrDispatchUnavailable, e.cause} }

func readinessRetriesExhausted(err error) bool {
	nodes, valid := machineAttachmentDialErrorTree(err)
	if !valid {
		return false
	}
	for _, node := range nodes {
		if _, ok := node.err.(*readinessRetryError); ok {
			return true
		}
	}
	return false
}
