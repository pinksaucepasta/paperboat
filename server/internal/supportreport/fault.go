package supportreport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"

	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
)

var ErrPanic = faultPanicError{}

type faultPanicError struct{}

func (faultPanicError) Error() string { return "panic recovered" }

const maxErrorChain = 8

// Driver and HTTP wrappers must not hide the originating native failure.
// Classification walks more nodes than the bounded list emitted in diagnostics.
const maxErrorNodes = 32

// Fault is the bounded, content-free projection shared by server logs, local
// support records, and Sentry. It deliberately has no error-message field.
type Fault struct {
	Component        string   `json:"component"`
	Operation        string   `json:"operation"`
	Stage            string   `json:"stage"`
	Code             string   `json:"code"`
	Cause            string   `json:"cause"`
	ErrorType        string   `json:"error_type"`
	SourceFile       string   `json:"source_file,omitempty"`
	SourceFunction   string   `json:"source_function,omitempty"`
	SourceLine       int      `json:"source_line,omitempty"`
	SupportReference string   `json:"support_reference,omitempty"`
	CorrelationID    string   `json:"correlation_id,omitempty"`
	ErrorChain       []string `json:"error_chain,omitempty"`
	Errno            int      `json:"errno,omitempty"`
	HTTPStatus       int      `json:"http_status,omitempty"`
	Outcome          string   `json:"outcome"`
	Severity         string   `json:"severity"`
}

// LogAttrs returns only the fixed, bounded diagnostic projection. Callers may
// attach these attributes to their owned logger without formatting the error.
func (f Fault) LogAttrs() []any {
	attrs := []any{
		"component", f.Component,
		"operation", f.Operation,
		"stage", f.Stage,
		"code", f.Code,
		"cause", f.Cause,
		"error_type", f.ErrorType,
		"source_file", f.SourceFile,
		"source_function", f.SourceFunction,
		"source_line", f.SourceLine,
		"error_chain", append([]string(nil), f.ErrorChain...),
		"errno", f.Errno,
	}
	if f.HTTPStatus >= 400 && f.HTTPStatus <= 599 {
		attrs = append(attrs, "http_status", f.HTTPStatus)
	}
	attrs = append(attrs,
		"support_reference", f.SupportReference,
		"correlation_id", f.CorrelationID,
		"outcome", f.Outcome,
		"severity", f.Severity,
	)
	return attrs
}

// diagnosticStatus is implemented by bounded provider/API error types and the
// HTTP response wrapper. It gives the projector status semantics without
// parsing Error() text.
type diagnosticStatus interface{ DiagnosticStatus() int }

type responseStatusError struct {
	status            int
	cause             error
	expectedRetryable bool
}

func (e responseStatusError) Error() string           { return "HTTP response failed" }
func (e responseStatusError) Unwrap() error           { return e.cause }
func (e responseStatusError) DiagnosticStatus() int   { return e.status }
func (e responseStatusError) ExpectedRetryable() bool { return e.expectedRetryable }

// WithDiagnosticStatus associates an already selected API response status with
// its original failure. The wrapper's Error method is fixed and never formats
// the wrapped error.
func WithDiagnosticStatus(status int, cause error) error {
	return responseStatusError{status: status, cause: cause}
}

// WithExpectedRetryableStatus marks a specific producer-owned response as an
// expected retryable condition. It keeps the cause traversable while allowing
// the reporter to retain local evidence without sending an exception event.
// Callers should use this only for finite typed/domain conditions that are
// deliberately returned as retryable responses; status alone is insufficient.
func WithExpectedRetryableStatus(status int, cause error) error {
	return responseStatusError{status: status, cause: cause, expectedRetryable: true}
}

type identityContextKey struct{}
type faultIdentity struct{ owner, request, reference string }

// WithFailureIdentity supplies already-authenticated owner and request IDs for
// local storage. Neither value becomes a Sentry tag or metric dimension.
func WithFailureIdentity(ctx context.Context, owner, request string, reference ...string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ref := ""
	if len(reference) > 0 && ValidReference(reference[0]) {
		ref = reference[0]
	}
	return context.WithValue(ctx, identityContextKey{}, faultIdentity{owner: owner, request: request, reference: ref})
}

// ProjectFault maps a wrapped Go error and producer-owned dimensions to a
// small fixed vocabulary. Unknown stages/codes collapse to safe values; error
// text and arbitrary error fields are never inspected.
func ProjectFault(ctx context.Context, component, operation, stage, code string, err error) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	if component != "paperboat-server" {
		component = "paperboat-server"
	}
	if !validOperation(operation) {
		operation = "http_other"
	}
	if !knownStage(stage) {
		stage = "other"
	}
	if !knownFaultCode(code) {
		code = "internal_error"
	}

	chain := safeErrorChain(err)
	errorType := "none"
	responseStatusErrorType := reflect.TypeOf(responseStatusError{}).PkgPath() + "." + reflect.TypeOf(responseStatusError{}).Name()
	for _, name := range chain {
		// HTTP status is a synthetic wrapper around the producer's failure.
		// Keep it in ErrorChain while exposing the underlying type as the more
		// useful, actionable ErrorType.
		if name != responseStatusErrorType {
			errorType = name
			break
		}
	}
	causeStatus, responseStatus := diagnosticHTTPStatuses(err)
	status := responseStatus
	if status == 0 {
		status = causeStatus
	}
	statusForCause := causeStatus
	if statusForCause == 0 {
		statusForCause = status
	}
	cause, errno := classifyCause(ctx, err, statusForCause)
	if errno == 0 {
		errno = firstErrno(err)
	}
	outcome, severity := "failed", "error"
	if cause == "context_canceled" {
		outcome, severity = "canceled", "info"
	} else if err == nil {
		outcome, severity = "state_change", "info"
		if code == "worker_recovered" {
			outcome = "success"
		}
	} else if responseStatus >= 400 && responseStatus < 500 && soleExpectedResponseFailure(err, false) {
		outcome, severity = "rejected", "warning"
	} else if status >= 500 && soleExpectedResponseFailure(err, true) {
		severity = "warning"
	}

	var reference string
	if sc, ok := ctx.Value(signalContextKey{}).(signalContext); ok && sc.service != nil && ValidReference(sc.reference) {
		reference = sc.reference
	} else if identity, ok := ctx.Value(identityContextKey{}).(faultIdentity); ok && ValidReference(identity.reference) {
		reference = identity.reference
	}
	correlation := serverdiagnostics.NormalizeRequestID(serverdiagnostics.CorrelationID(ctx))
	sourceFile, sourceFunction, sourceLine := sourceCaller(1)
	return Fault{
		Component: component, Operation: operation, Stage: stage, Code: code,
		Cause: cause, ErrorType: errorType, SourceFile: sourceFile, SourceFunction: sourceFunction, SourceLine: sourceLine, SupportReference: reference,
		CorrelationID: correlation, ErrorChain: chain, Errno: errno,
		HTTPStatus: boundedHTTPStatus(status),
		Outcome:    outcome, Severity: severity,
	}
}

// Response annotations own one failure branch. They cannot suppress an
// independent joined failure or an incomplete/cyclic error tree.
func soleExpectedResponseFailure(err error, retryable bool) bool {
	if !retryable {
		cause, _ := classifyCause(context.Background(), err, 0)
		if cause != "internal" && cause != "none" && cause != "context_canceled" {
			return false
		}
	}
	seen := make(map[error]struct{}, maxErrorChain)
	annotated := false
	for depth := 0; err != nil && depth < maxErrorChain; depth++ {
		if nilDiagnosticError(err) {
			return false
		}
		if reflect.ValueOf(err).Comparable() {
			if _, exists := seen[err]; exists {
				return false
			}
			seen[err] = struct{}{}
		}
		if retryable {
			if classified, ok := err.(interface{ ExpectedRetryable() bool }); ok && classified.ExpectedRetryable() {
				annotated = true
			}
		} else if classified, ok := err.(diagnosticStatus); ok {
			status := classified.DiagnosticStatus()
			if status >= 500 {
				return false
			}
			annotated = annotated || status >= 400 && status < 500
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return annotated
		}
		err = wrapped.Unwrap()
		if err == nil {
			return annotated
		}
	}
	return false
}

func sourceCaller(skip int) (string, string, int) {
	pc, file, line, ok := runtime.Caller(skip + 1)
	if !ok || line < 1 || line > 1_000_000 {
		return "", "", 0
	}
	name := ""
	if function := runtime.FuncForPC(pc); function != nil {
		name = function.Name()
	}
	return safeSourceFile(filepath.Base(file)), safeSourceFunction(name), line
}

func safeSourceFile(value string) string {
	if value == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			out.WriteRune(r)
		}
		if out.Len() >= 128 {
			break
		}
	}
	return out.String()
}

func safeSourceFunction(value string) string {
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		value = value[slash+1:]
	}
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' {
			out.WriteRune(r)
		}
		if out.Len() >= 64 {
			break
		}
	}
	return out.String()
}

func diagnosticHTTPStatuses(err error) (causeStatus, responseStatus int) {
	for _, current := range safeErrorNodes(err) {
		if value, ok := current.(responseStatusError); ok {
			if value.status >= 100 && value.status <= 599 {
				responseStatus = value.status
			}
			continue
		}
		if value, ok := current.(diagnosticStatus); ok {
			status := value.DiagnosticStatus()
			if status >= 100 && status <= 599 {
				if causeStatus == 0 {
					causeStatus = status
				}
				if responseStatus == 0 {
					responseStatus = status
				}
			}
		}
	}
	if responseStatus == 0 {
		responseStatus = causeStatus
	}
	return causeStatus, responseStatus
}

func classifyCause(ctx context.Context, err error, status int) (string, int) {
	if matchesError(err, context.DeadlineExceeded) || matchesError(ctx.Err(), context.DeadlineExceeded) {
		return "deadline_exceeded", 0
	}
	for _, current := range safeErrorNodes(err) {
		switch value := current.(type) {
		case interface{ DiagnosticErrno() int }:
			if number := value.DiagnosticErrno(); number > 0 && number <= 65535 {
				return "system_error", number
			}
		case *net.DNSError:
			if value.IsNotFound {
				return "dns_not_found", 0
			}
			if value.IsTimeout {
				return "dns_timeout", 0
			}
			if value.IsTemporary {
				return "dns_temporary", 0
			}
			return "dns_failure", 0
		case *x509.UnknownAuthorityError:
			return "tls_unknown_authority", 0
		case x509.HostnameError:
			return "tls_hostname", 0
		case *tls.CertificateVerificationError:
			return "tls_verification", 0
		case syscall.Errno:
			return errnoCause(value), boundedErrno(value)
		case net.Error:
			if value.Timeout() {
				return "network_timeout", 0
			}
		}
		if stateError, ok := current.(interface{ SQLState() string }); ok {
			if cause := postgresCause(stateError.SQLState()); cause != "" {
				return cause, 0
			}
		}
	}

	switch {
	case matchesError(err, os.ErrNotExist), matchesError(err, syscall.ENOENT):
		return "not_found", 0
	case matchesError(err, os.ErrPermission), matchesError(err, syscall.EACCES), matchesError(err, syscall.EPERM):
		return "permission_denied", 0
	case matchesError(err, syscall.ECONNREFUSED):
		return "connection_refused", int(syscall.ECONNREFUSED)
	case matchesError(err, syscall.EHOSTUNREACH), matchesError(err, syscall.ENETUNREACH):
		return "network_unreachable", firstErrno(err)
	case matchesError(err, syscall.ECONNRESET):
		return "connection_reset", int(syscall.ECONNRESET)
	case matchesError(err, syscall.ETIMEDOUT):
		return "network_timeout", int(syscall.ETIMEDOUT)
	case matchesError(err, syscall.EPIPE):
		return "broken_pipe", int(syscall.EPIPE)
	case matchesError(err, syscall.EEXIST):
		return "conflict", int(syscall.EEXIST)
	case matchesError(err, syscall.ENOSPC), matchesError(err, syscall.EMFILE), matchesError(err, syscall.ENFILE):
		return "resource_exhausted", firstErrno(err)
	case matchesError(err, net.ErrClosed):
		return "connection_closed", 0
	case matchesError(err, io.EOF):
		return "end_of_stream", 0
	case matchesError(err, io.ErrUnexpectedEOF):
		return "truncated_stream", 0
	}
	if status >= 400 && status <= 599 {
		return causeForHTTPStatus(status), 0
	}
	if (matchesError(err, context.Canceled) || matchesError(ctx.Err(), context.Canceled)) && !hasNonWrapperErrorLeaf(err) {
		return "context_canceled", 0
	}
	if err == nil {
		return "none", 0
	}
	return "internal", 0
}

func hasNonWrapperErrorLeaf(err error) bool {
	for _, current := range safeErrorNodes(err) {
		if matchesError(current, context.Canceled) || matchesError(current, context.DeadlineExceeded) {
			continue
		}
		switch current.(type) {
		case interface{ Unwrap() error }, interface{ Unwrap() []error }:
			continue
		default:
			return true
		}
	}
	return false
}

func causeForHTTPStatus(status int) string {
	switch status {
	case 401, 403:
		return "permission_denied"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 503:
		return "service_unavailable"
	default:
		if status >= 400 && status < 500 {
			return "invalid_request"
		}
		if status >= 500 && status <= 599 {
			return "upstream_failure"
		}
		return "internal"
	}
}

func boundedHTTPStatus(status int) int {
	if status < 400 || status > 599 {
		return 0
	}
	return status
}

func postgresCause(state string) string {
	switch state {
	case "23505":
		return "postgres_unique_violation"
	case "23503":
		return "postgres_foreign_key"
	case "23514":
		return "postgres_check_violation"
	case "40001":
		return "postgres_serialization"
	case "40P01":
		return "postgres_deadlock"
	case "55P03":
		return "postgres_lock_unavailable"
	case "57014":
		return "postgres_query_canceled"
	case "08000", "08003", "08006", "08001", "08004", "08007", "08P01":
		return "postgres_connection"
	case "28P01", "28000":
		return "postgres_authentication"
	case "":
		return ""
	default:
		return "postgres_error"
	}
}

func errnoCause(value syscall.Errno) string {
	switch value {
	case syscall.ECONNREFUSED:
		return "connection_refused"
	case syscall.EHOSTUNREACH, syscall.ENETUNREACH:
		return "network_unreachable"
	case syscall.ECONNRESET:
		return "connection_reset"
	case syscall.ETIMEDOUT:
		return "network_timeout"
	case syscall.EPIPE:
		return "broken_pipe"
	case syscall.EADDRINUSE:
		return "address_in_use"
	case syscall.EINVAL:
		return "invalid_argument"
	case syscall.ENOENT:
		return "not_found"
	case syscall.EACCES, syscall.EPERM:
		return "permission_denied"
	case syscall.EEXIST:
		return "conflict"
	case syscall.ENOSPC, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM:
		return "resource_exhausted"
	default:
		return "system_error"
	}
}

func boundedErrno(value syscall.Errno) int {
	if value == 0 || value > 65535 {
		return 0
	}
	return int(value)
}

func firstErrno(err error) int {
	for _, current := range safeErrorNodes(err) {
		if value, ok := current.(syscall.Errno); ok {
			return boundedErrno(value)
		}
		if value, ok := current.(interface{ DiagnosticErrno() int }); ok {
			if number := value.DiagnosticErrno(); number > 0 && number <= 65535 {
				return number
			}
		}
	}
	return 0
}

func matchesError(err, target error) bool {
	if target == nil {
		return err == nil
	}
	for _, current := range safeErrorNodes(err) {
		currentType := reflect.TypeOf(current)
		targetType := reflect.TypeOf(target)
		if currentType != nil && targetType != nil && reflect.ValueOf(current).Comparable() && reflect.ValueOf(target).Comparable() && current == target {
			return true
		}
		if matcher, ok := current.(interface{ Is(error) bool }); ok && matcher.Is(target) {
			return true
		}
	}
	return false
}

func safeErrorChain(err error) []string {
	nodes := safeErrorNodes(err)
	if len(nodes) > maxErrorChain {
		nodes = nodes[:maxErrorChain]
	}
	chain := make([]string, 0, len(nodes))
	for _, current := range nodes {
		name := safeErrorType(current)
		if name != "" {
			chain = append(chain, name)
		}
	}
	return chain
}

func safeErrorType(err error) string {
	if err == nil {
		return ""
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		name = "error"
	}
	if path := t.PkgPath(); path != "" {
		name = path + "." + name
	}
	if len(name) > 128 {
		return "error"
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("_./-", r) {
			return "error"
		}
	}
	return name
}

func safeErrorNodes(err error) []error {
	if err == nil {
		return nil
	}
	queue := []error{err}
	seen := make(map[error]struct{}, maxErrorNodes)
	nodes := make([]error, 0, maxErrorNodes)
	for len(queue) > 0 && len(nodes) < maxErrorNodes {
		current := queue[0]
		queue = queue[1:]
		if current == nil || nilDiagnosticError(current) {
			continue
		}
		if reflect.ValueOf(current).Comparable() {
			if _, ok := seen[current]; ok {
				continue
			}
			seen[current] = struct{}{}
		}
		nodes = append(nodes, current)
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			remaining := maxErrorNodes - len(nodes) - len(queue)
			if remaining > 0 {
				if len(children) > remaining {
					children = children[:remaining]
				}
				queue = append(queue, children...)
			}
		} else if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			queue = append(queue, wrapped.Unwrap())
		}
	}
	return nodes
}

func nilDiagnosticError(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}

func knownStage(value string) bool {
	return strings.Contains(" "+knownFaultStages+" ", " "+value+" ")
}

const knownFaultStages = `access_session_retention edge_set_reconcile backup_execution backup_verification backup_restore backup_cleanup backup_reporting http_handler http_response panic panic_recovery worker_exit worker_iteration worker_recovery service_shutdown service_startup cleanup report_cleanup revocations distribution_cleanup reconciliation readiness route_assignment stale_node edge_dns pending_revocation terminal_session_operation terminal_session_cleanup stale_machine_offline warning_reconciliation config_revocation preview_lease tunnel_expiry connector_rotation connector_drain domain_dns preview_dns preview_outbox team_retention team_entitlements team_billing team_inbox certificate_cleanup platform_certificate_cleanup diagnostic_upload_cleanup support_cleanup provider_call service_error team_retention_cleanup team_entitlement_expiry team_billing_reconcile edge_dns_publication diagnostic_upload_cleanup platform_certificate_revocations platform_certificate_cleanup platform_certificate_reconciliation certificate_redistribution certificate_revocations certificate_distribution_cleanup certificate_reconciliation stale_edge_node edge_route_assignment authorize handler retry service`

const knownFaultCodes = `access_session_retention_failed edge_set_reconcile_failed usage_unavailable access_invalid access_uncertain activation_limit ambiguous_team_access assignment_clear_failed auth_callback_failed auth_session_touch_failed authentication_required authorization_unavailable availability_unavailable availability_version_conflict backup_report_conflict backup_report_unauthorized bandwidth_quota_exhausted billing_operation_failed billing_pending billing_reconciliation_required billing_unavailable capabilities_unavailable capabilities_version_conflict checkout_expired client_update_policy_unavailable config_writes_disabled configuration_unavailable conflict_resolution_invalid connector_activation_unavailable connector_unavailable credential_invalid credential_unavailable csrf_failed diagnostic_upload_expired diagnostic_upload_mismatch diagnostic_upload_not_found diagnostic_upload_unavailable diagnostics_unavailable duplicate_machine_identity edge_identity_invalid edge_inventory_unavailable edge_policy_transition_required favorite_limit_reached favorite_resource_not_found file_transfer_unavailable forbidden generation_conflict github_connection_unavailable github_identity_conflict github_oauth_failed github_required github_scope_denied helper_machine_unavailable idempotency_conflict idempotency_key_conflict idempotency_key_required infrastructure_conflict infrastructure_unavailable inspector_capacity_reached inspector_not_authorized inspector_unavailable installation_failure_not_recorded insufficient_scope interactive_confirmation_required internal_error invalid_browser_key invalid_credentials invalid_favorite invalid_inspector_request invalid_machine_name invalid_machine_setup invalid_operation_id invalid_policy invalid_request invalid_return_to invalid_signature invalid_support_lookup invalid_support_reference invalid_terminal_access_policy invalid_terminal_code_access invalid_terminal_session_name invalid_user_machine_pairing invalid_usage_query invalid_usage_report invalid_workspace invitation_unavailable lazy_registration_denied lease_busy lease_invalid lease_lost machine_capability_unavailable machine_control_bootstrap_invalid machine_control_credential_invalid machine_control_proof_invalid machine_identity_conflict machine_identity_invalid machine_identity_required machine_installation_failed machine_installation_invalid machine_installation_unavailable machine_name_conflict machine_not_found machine_not_ready machine_offline machine_plan_inactive machine_provisioning_unavailable machine_publication_active machine_limit_reached preview_limit_reached browser_terminal_limit_reached machine_rename_failed machine_seat_unavailable machine_services_conflict machine_services_denied machine_services_unavailable machine_setup_failed maintenance_approval_not_actionable maintenance_approval_not_found maintenance_approval_unavailable managed_ssh_key_capacity native_access_widening_denied native_private_unavailable not_found not_found_or_forbidden oauth_state_failed operation_conflict operation_not_recoverable operation_retry_window_active operator_audit_unavailable operator_conflict operator_evidence_unavailable operator_not_found operator_result_unavailable operator_unavailable origin_failed payment_required peer_network_conflict peer_network_denied peer_network_unavailable policy_changed policy_limit policy_unavailable product_unavailable provider_outcome_unknown provider_unavailable recovery_unavailable release_authority_bundle_invalid release_authority_import_conflict release_authority_signature_invalid release_unavailable repository_candidates_unavailable repository_read_access_unavailable rotation_required runtime_invalid sentry_capability_unsupported sentry_not_configured sentry_rate_limited setup_complete status_invalid status_revision_stale subscription_exists support_unavailable synthetic_failure team_changed team_id_reserved team_inbox_conflict team_inbox_declined team_inbox_expired team_inbox_forbidden team_inbox_limit team_inbox_pending team_inbox_recipient_offline team_inbox_request_not_found team_inbox_revoked team_inbox_unavailable team_limit team_subscription_active temporarily_unavailable terminal_access_policy_changed terminal_access_required terminal_code_access_changed terminal_code_rate_limited terminal_code_unavailable terminal_guest_capacity terminal_host_unavailable terminal_join_denied terminal_join_unavailable terminal_session_closed terminal_session_limit terminal_session_limit_reached terminal_session_name_conflict terminal_session_not_closed terminal_session_not_found terminal_session_reserved terminal_unavailable transfer_destination_default_failed transfer_destination_session_failed transfer_destination_unavailable transfer_destinations_unavailable unauthenticated update_observation_unavailable user_machine_approval_pending user_machine_delete_failed user_machine_enrollment_not_cancellable user_machine_enrollment_not_found user_machine_enrollment_not_retryable user_machine_enrollment_start_failed user_machine_enrollment_audit_failed user_machine_installation_unavailable user_machine_not_found user_machine_pairing_denied user_machine_pairing_expired user_machine_pairing_failed user_machine_update_status_not_found validation_failed version_conflict viewer_policy_conflict viewer_policy_owner_changed viewer_policy_unavailable workspace_target_conflict workspace_unavailable carrier_unavailable connector_access_forbidden connector_control_invalid connector_control_unavailable connector_credential_proof_invalid connector_session_stale generation_conflict idempotency_conflict idempotency_required if_match_required invalid_content_type invalid_cursor invalid_etag invalid_idempotency_key machine_identity_required operation_in_progress operation_not_cancellable operation_not_found publication_not_granted team_subscription_required tunnel_conflict tunnel_deleted tunnel_name_conflict tunnel_not_found attachment_conflict attachment_expired edge_admission_unavailable stale_binding unsafe_event_metadata worker_failed worker_panic connector_rotation_failed connector_drain_failed worker_recovered worker_iteration_failed worker_canceled control_sync_failed http_server_failed service_failed support_cleanup_failed machine_revocation_failed terminal_operation_failed terminal_cleanup_failed terminal_orphan_reconcile_failed machine_offline_reconcile_failed team_retention_failed team_entitlements_failed edge_dns_reconcile_failed diagnostic_cleanup_failed platform_certificate_revocation_failed platform_certificate_cleanup_failed platform_certificate_reconcile_failed certificate_redistribution_failed certificate_revocation_failed certificate_cleanup_failed certificate_generation_conflict stale_node_reconcile_failed edge_route_reconcile_failed panic control_unavailable connector_control_failed invalid_input malformed_frame frame_too_large protocol_incompatible capability_missing authentication_failed credential_expired identity_mismatch session_conflict session_closed lease_expired heartbeat_timeout snapshot_required generation_gap stale_generation content_hash_mismatch snapshot_rejected delta_rejected not_ready canceled stale_session unsupported_message drain_rejected drain_timeout credential_rotation_rejected credential_rotation_not_ready credential_rotation_failed sentry_delivery_failed api_token_invalid api_token_name_taken api_token_limit invalid_api_token_scope outside_token_scope policy_changes_unconfirmed policy_version_conflict policy_edit_forbidden policy_explain_forbidden api_token_required`

func knownFaultCode(value string) bool {
	auxiliaryObservationCodes := " environment_observation_unavailable availability_observation_unavailable capabilities_observation_unavailable "
	return value == "support_storage_failed" || value == "selfhost_unavailable" || strings.Contains(auxiliaryObservationCodes, " "+value+" ") || strings.Contains(" "+knownFaultCodes+" ", " "+value+" ")
}

func validFaultOutcome(value string) bool {
	return value == "success" || value == "failed" || value == "rejected" || value == "canceled" || value == "state_change"
}

func validFaultSeverity(value string) bool {
	return value == "info" || value == "warning" || value == "error"
}

func safeErrorTypeValue(value string) string {
	if value == "none" {
		return value
	}
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("_./-", r) {
			return ""
		}
	}
	return value
}

func knownCause(value string) bool {
	return strings.Contains(" "+knownFaultCauses+" ", " "+value+" ")
}

const knownFaultCauses = `none internal context_canceled deadline_exceeded dns_not_found dns_timeout dns_temporary dns_failure tls_unknown_authority tls_hostname tls_verification network_timeout network_unreachable not_found permission_denied conflict rate_limited service_unavailable invalid_request upstream_failure connection_refused connection_reset connection_closed broken_pipe end_of_stream truncated_stream address_in_use invalid_argument resource_exhausted system_error postgres_unique_violation postgres_foreign_key postgres_check_violation postgres_serialization postgres_deadlock postgres_lock_unavailable postgres_query_canceled postgres_connection postgres_authentication postgres_error`
