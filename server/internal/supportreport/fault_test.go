package supportreport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
)

type privateDiagnosticError struct {
	cause error
	calls int
}

func (e *privateDiagnosticError) Error() string {
	e.calls++
	return "SECRET diagnostic text"
}
func (e *privateDiagnosticError) Unwrap() error { return e.cause }

type sqlStateError struct{ state string }

func (e sqlStateError) Error() string    { return "SECRET database text" }
func (e sqlStateError) SQLState() string { return e.state }

type diagnosticStatusError struct {
	status int
	cause  error
}

func (e diagnosticStatusError) Error() string         { return "upstream response failed" }
func (e diagnosticStatusError) Unwrap() error         { return e.cause }
func (e diagnosticStatusError) DiagnosticStatus() int { return e.status }

type cyclicDiagnosticError struct{ next error }

func (e *cyclicDiagnosticError) Error() string { return "SECRET cycle text" }
func (e *cyclicDiagnosticError) Unwrap() error { return e.next }

type joinedDiagnosticError struct{ children []error }

func (e joinedDiagnosticError) Error() string   { return "SECRET joined text" }
func (e joinedDiagnosticError) Unwrap() []error { return e.children }

func TestProjectFaultPreservesBoundedCauseWithoutFormattingErrors(t *testing.T) {
	wrapped := &privateDiagnosticError{cause: syscall.ETIMEDOUT}
	ctx := serverdiagnostics.WithCorrelationID(context.Background(), "request_123")
	ctx = WithFailureIdentity(ctx, "user_a", "request_123", NewReference())
	fault := ProjectFault(ctx, "paperboat-server", "http_billing", "http_handler", "billing_unavailable", WithDiagnosticStatus(503, wrapped))
	if wrapped.calls != 0 {
		t.Fatalf("projector formatted a private error %d times", wrapped.calls)
	}
	if fault.Cause != "network_timeout" || !strings.HasSuffix(fault.ErrorType, ".privateDiagnosticError") || fault.Errno != int(syscall.ETIMEDOUT) {
		t.Fatalf("underlying cause was not preserved: %+v", fault)
	}
	if fault.Outcome != "failed" || fault.Severity != "error" || fault.HTTPStatus != 503 || !ValidReference(fault.SupportReference) || fault.CorrelationID != "request_123" {
		t.Fatalf("safe request identity/status was lost: %+v", fault)
	}
	if strings.Contains(fmt.Sprint(fault), "SECRET") {
		t.Fatalf("fault contains private error text: %+v", fault)
	}
}

func TestProjectFaultSuppressesOnlyExplicitExpectedRetryableStatus(t *testing.T) {
	unexpected := ProjectFault(context.Background(), "paperboat-server", "http_edge", "http_handler", "control_unavailable", WithDiagnosticStatus(503, syscall.ETIMEDOUT))
	if unexpected.Cause != "network_timeout" || unexpected.HTTPStatus != 503 || unexpected.Outcome != "failed" || unexpected.Severity != "error" {
		t.Fatalf("unexpected 503 was not retained as a failure: %+v", unexpected)
	}
	expected := ProjectFault(context.Background(), "paperboat-server", "http_previews", "http_handler", "edge_admission_unavailable", WithExpectedRetryableStatus(503, syscall.ETIMEDOUT))
	if expected.Cause != "network_timeout" || expected.HTTPStatus != 503 || expected.Outcome != "failed" || expected.Severity != "warning" {
		t.Fatalf("typed retryable 503 lost local cause or was treated as an exception: %+v", expected)
	}
}

func TestProjectFaultKeepsUpstreamCauseStatusSeparateFromAPIStatus(t *testing.T) {
	fault := ProjectFault(context.Background(), "paperboat-server", "http_billing", "http_handler", "provider_unavailable", WithDiagnosticStatus(503, diagnosticStatusError{status: 502, cause: syscall.ECONNREFUSED}))
	if fault.Cause != "connection_refused" || fault.HTTPStatus != 503 || fault.Severity != "error" {
		t.Fatalf("upstream causal status/cause was obscured by the API status: %+v", fault)
	}
	statusOnly := ProjectFault(context.Background(), "paperboat-server", "http_billing", "http_handler", "provider_unavailable", WithDiagnosticStatus(501, errors.New("private")))
	if statusOnly.Cause != "upstream_failure" || statusOnly.HTTPStatus != 501 {
		t.Fatalf("plain API status did not classify as failure: %+v", statusOnly)
	}
}

func TestProjectFaultClassifiesDatabaseAndCancellation(t *testing.T) {
	database := ProjectFault(context.Background(), "paperboat-server", "database_operation", "service_error", "internal_error", sqlStateError{state: "40P01"})
	if database.Cause != "postgres_deadlock" || !strings.HasSuffix(database.ErrorType, ".sqlStateError") {
		t.Fatalf("PostgreSQL state not classified: %+v", database)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := ProjectFault(ctx, "paperboat-server", "worker_machines", "worker_iteration", "worker_canceled", fmt.Errorf("worker stopped: %w", context.Canceled))
	if canceled.Cause != "context_canceled" || canceled.Outcome != "canceled" || canceled.Severity != "info" {
		t.Fatalf("cancellation was not treated as expected: %+v", canceled)
	}
}

func TestProjectFaultSubstantiveCauseOutranksCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "connection refused", err: errors.Join(context.Canceled, syscall.ECONNREFUSED), want: "connection_refused"},
		{name: "unknown leaf", err: errors.Join(context.Canceled, errors.New("private operation failure")), want: "internal"},
		{name: "deadline", err: errors.Join(context.Canceled, context.DeadlineExceeded), want: "deadline_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault := ProjectFault(context.Background(), "paperboat-server", "worker_machines", "worker_iteration", "worker_failed", tc.err)
			if fault.Cause != tc.want || fault.Outcome != "failed" || fault.Severity != "error" {
				t.Fatalf("substantive failure was masked by cancellation: %+v", fault)
			}
		})
	}
}

func TestProjectFaultKeepsDeadlineAsFailureAndProjectsHTTPStatusSeparately(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	fault := ProjectFault(ctx, "paperboat-server", "http_billing", "http_handler", "billing_unavailable", context.DeadlineExceeded)
	if fault.Cause != "deadline_exceeded" || fault.Outcome != "failed" || fault.Severity != "error" {
		t.Fatalf("deadline was incorrectly treated as cancellation: %+v", fault)
	}
	status := ProjectFault(context.Background(), "paperboat-server", "http_other", "http_handler", "internal_error", WithDiagnosticStatus(501, errors.New("private")))
	if status.Cause != "upstream_failure" || status.HTTPStatus != 501 {
		t.Fatalf("HTTP status was not separately and safely projected: %+v", status)
	}
	for _, tc := range []struct {
		status int
		cause  string
	}{{401, "permission_denied"}, {404, "not_found"}, {409, "conflict"}, {429, "rate_limited"}, {503, "service_unavailable"}, {422, "invalid_request"}, {500, "upstream_failure"}} {
		fault := ProjectFault(context.Background(), "paperboat-server", "http_other", "http_handler", "internal_error", WithDiagnosticStatus(tc.status, nil))
		if fault.Cause != tc.cause || fault.HTTPStatus != tc.status {
			t.Errorf("status %d projected as cause=%q http_status=%d, want %q and %d", tc.status, fault.Cause, fault.HTTPStatus, tc.cause, tc.status)
		}
	}
}

func TestProjectFaultBoundsCyclesAndUnknownDimensions(t *testing.T) {
	cycle := &cyclicDiagnosticError{}
	cycle.next = cycle
	fault := ProjectFault(context.Background(), "untrusted-component", "untrusted_operation", "secret-stage", "secret-code", cycle)
	if fault.Component != "paperboat-server" || fault.Operation != "http_other" || fault.Stage != "other" || fault.Code != "internal_error" {
		t.Fatalf("untrusted dimensions survived projection: %+v", fault)
	}
	if len(fault.ErrorChain) != 1 || !strings.HasSuffix(fault.ErrorType, ".cyclicDiagnosticError") {
		t.Fatalf("cycle was not cut safely: %+v", fault)
	}
	children := make([]error, 64)
	for i := range children {
		children[i] = fmt.Errorf("wrapped: %w", errors.New("private"))
	}
	joined := ProjectFault(context.Background(), "paperboat-server", "http_other", "http_handler", "internal_error", joinedDiagnosticError{children: children})
	if len(joined.ErrorChain) > maxErrorChain {
		t.Fatalf("error chain exceeded bound: %d", len(joined.ErrorChain))
	}
}

func TestFaultOmitsHTTPStatusUnlessItIsAnErrorResponse(t *testing.T) {
	for _, status := range []int{0, 202, 399, 600} {
		fault := Fault{HTTPStatus: status, ErrorChain: []string{"syscall.Errno"}, Outcome: "failed", Severity: "error"}
		for i, attrs := 0, fault.LogAttrs(); i < len(attrs); i += 2 {
			if attrs[i] == "http_status" {
				t.Fatalf("status %d emitted as a local HTTP status attribute", status)
			}
		}
		if _, ok := faultTags(fault)["http_status"]; ok {
			t.Fatalf("status %d emitted as a Sentry HTTP status tag", status)
		}
		ctx := faultContext(fault)
		if _, ok := ctx["http_status"]; ok {
			t.Fatalf("status %d emitted as a Sentry HTTP status context", status)
		}
		projected := safeFaultContext(ctx)
		if projected == nil {
			t.Fatalf("Sentry fault context without an HTTP failure was discarded for status %d", status)
		}
	}

	fault := Fault{HTTPStatus: 503, ErrorChain: []string{"syscall.Errno"}, Outcome: "failed", Severity: "error"}
	attrs := fault.LogAttrs()
	found := false
	for i := 0; i < len(attrs); i += 2 {
		if attrs[i] == "http_status" && attrs[i+1] == 503 {
			found = true
		}
	}
	if !found || faultTags(fault)["http_status"] != "503" || faultContext(fault)["http_status"] != 503 {
		t.Fatal("valid error HTTP status was omitted from fault metadata")
	}
}

func TestResponseAnnotationCannotHideIndependentOperationalFailure(t *testing.T) {
	for _, err := range []error{
		WithDiagnosticStatus(401, errors.Join(errors.New("private rejection"), syscall.EIO)),
		errors.Join(WithDiagnosticStatus(401, errors.New("private rejection")), syscall.EIO),
		WithDiagnosticStatus(403, syscall.EIO),
		WithExpectedRetryableStatus(503, errors.Join(context.DeadlineExceeded, syscall.EIO)),
		errors.Join(WithExpectedRetryableStatus(503, context.DeadlineExceeded), syscall.EIO),
	} {
		fault := ProjectFault(context.Background(), "paperboat-server", "http_edge", "http_handler", "authorization_unavailable", err)
		if fault.Outcome != "failed" || fault.Severity != "error" {
			t.Fatalf("independent operational cause suppressed for %T", err)
		}
	}
	cycle := &cyclicDiagnosticError{}
	cycle.next = cycle
	for _, err := range []error{WithDiagnosticStatus(401, cycle), WithDiagnosticStatus(401, (*cyclicDiagnosticError)(nil))} {
		fault := ProjectFault(context.Background(), "paperboat-server", "http_edge", "http_handler", "authorization_unavailable", err)
		if fault.Outcome != "failed" || fault.Severity != "error" {
			t.Fatal("incomplete rejection proof suppressed a failure")
		}
	}
	pure := ProjectFault(context.Background(), "paperboat-server", "http_edge", "http_handler", "unauthenticated", WithDiagnosticStatus(401, errors.New("private rejection")))
	if pure.Outcome != "rejected" || pure.Severity != "warning" {
		t.Fatal("sole producer-owned rejection changed behavior")
	}
}
