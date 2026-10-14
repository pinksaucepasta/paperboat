package workloadbridge

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/session"

// ownerHTTPFailure preserves only the private bridge response status. The
// response body may contain private application data and is never an error.
type ownerHTTPFailure struct{ status int }

func (ownerHTTPFailure) Error() string           { return session.ErrOwnerUnavailable.Error() }
func (ownerHTTPFailure) Is(target error) bool    { return target == session.ErrOwnerUnavailable }
func (e ownerHTTPFailure) DiagnosticStatus() int { return e.status }
func (ownerHTTPFailure) DiagnosticStage() string { return "target_connect" }
func (ownerHTTPFailure) DiagnosticErrno() int    { return 0 }
func (e ownerHTTPFailure) DiagnosticCause() string {
	switch e.status {
	case 401, 403:
		return "permission_denied"
	case 404:
		return "not_found"
	case 408, 504:
		return "network_timeout"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 502, 503:
		return "service_unavailable"
	}
	if e.status >= 500 {
		return "upstream_failure"
	}
	return "invalid_request"
}

type ownerResponseLimitFailure struct{}

func (ownerResponseLimitFailure) Error() string           { return session.ErrOwnerUnavailable.Error() }
func (ownerResponseLimitFailure) Unwrap() error           { return session.ErrOwnerUnavailable }
func (ownerResponseLimitFailure) DiagnosticStage() string { return "delivery" }
func (ownerResponseLimitFailure) DiagnosticCause() string { return "resource_exhausted" }
func (ownerResponseLimitFailure) DiagnosticErrno() int    { return 0 }
