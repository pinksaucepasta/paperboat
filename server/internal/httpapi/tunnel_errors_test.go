package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/publicationdns"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestTunnelErrorPureTypedDomainOutcomes(t *testing.T) {
	tests := []struct {
		name, code, outcome, repairAction string
		err                               error
		status                            int
		retryable                         bool
	}{
		{name: "team subscription", err: previewtunnelstore.ErrTeamSubscriptionRequired, status: http.StatusForbidden, code: "team_subscription_required", outcome: "unchanged", repairAction: "manage_team_billing"},
		{name: "machine publication", err: tunnelv1.ErrMachinePublicationDenied, status: http.StatusForbidden, code: "publication_not_granted", outcome: "unchanged", repairAction: "use_target_local_owner"},
		{name: "if match required", err: previewtunnelapi.ErrIfMatchRequired, status: http.StatusPreconditionRequired, code: "if_match_required", outcome: "unchanged", repairAction: "fetch_current_tunnel"},
		{name: "invalid etag", err: previewtunnelapi.ErrInvalidETag, status: http.StatusBadRequest, code: "invalid_etag", outcome: "unchanged", repairAction: "fetch_current_tunnel"},
		{name: "invalid cursor", err: previewtunnelapi.ErrInvalidCursor, status: http.StatusBadRequest, code: "invalid_cursor", outcome: "unchanged", repairAction: "restart_pagination"},
		{name: "idempotency required", err: previewtunnelapi.ErrIdempotencyRequired, status: http.StatusBadRequest, code: "idempotency_required", outcome: "unchanged", repairAction: "retry_with_idempotency_key"},
		{name: "invalid idempotency", err: previewtunnelapi.ErrInvalidIdempotency, status: http.StatusBadRequest, code: "invalid_idempotency_key", outcome: "unchanged", repairAction: "send_valid_idempotency_key"},
		{name: "host actor required", err: previewtunnelapi.ErrHostActorRequired, status: http.StatusForbidden, code: "forbidden", outcome: "unchanged", repairAction: "authenticate_with_required_host"},
		{name: "forbidden", err: previewtunnelapi.ErrForbidden, status: http.StatusForbidden, code: "forbidden", outcome: "unchanged", repairAction: "authenticate_with_required_host"},
		{name: "host not found", err: tunnelv1.ErrHostNotFound, status: http.StatusForbidden, code: "forbidden", outcome: "unchanged", repairAction: "authenticate_with_required_host"},
		{name: "not found", err: tunnelv1.ErrNotFound, status: http.StatusNotFound, code: "tunnel_not_found", outcome: "unchanged", repairAction: "refresh"},
		{name: "generation conflict", err: tunnelv1.ErrGenerationConflict, status: http.StatusPreconditionFailed, code: "generation_conflict", outcome: "unchanged", repairAction: "fetch_current_tunnel"},
		{name: "idempotency conflict", err: tunnelv1.ErrIdempotencyConflict, status: http.StatusConflict, code: "idempotency_conflict", outcome: "unchanged", repairAction: "retry_with_new_idempotency_key"},
		{name: "conflict", err: tunnelv1.ErrConflict, status: http.StatusConflict, code: "tunnel_conflict", outcome: "unchanged", repairAction: "refresh_and_retry"},
		{name: "name conflict", err: tunnelv1.ErrNameConflict, status: http.StatusConflict, code: "tunnel_name_conflict", outcome: "unchanged", repairAction: "choose_another_name"},
		{name: "terminal state", err: tunnelv1.ErrTerminalState, status: http.StatusConflict, code: "tunnel_deleted", outcome: "unchanged", repairAction: "create_new_tunnel"},
		{name: "operation in progress", err: tunnelv1.ErrOperationInProgress, status: http.StatusConflict, code: "operation_in_progress", outcome: "uncertain", retryable: true, repairAction: "inspect_operation"},
		{name: "invalid input", err: tunnelv1.ErrInvalidInput, status: http.StatusBadRequest, code: "invalid_request", outcome: "unchanged", repairAction: "fix_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _, reference := diagnosticRequest("/v1/tunnels/tun_status_01")
			response := httptest.NewRecorder()
			writeTunnelError(response, request, test.err)
			if response.Code != test.status {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code || body.Error.Outcome != test.outcome || body.Error.Retryable == nil || *body.Error.Retryable != test.retryable || body.Error.RepairAction != test.repairAction || body.Error.SupportReference != reference {
				t.Fatalf("typed outcome changed: %+v", body.Error)
			}
		})
	}
}

func TestTunnelErrorKeepsPureWorkspaceDenialAndReportsMixedFailure(t *testing.T) {
	denied := httptest.NewRecorder()
	writeTunnelError(denied, httptest.NewRequest(http.MethodPost, "/v1/tunnels", nil), workspace.ErrForbidden)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("pure workspace denial status=%d, want 403: %s", denied.Code, denied.Body.String())
	}

	const privateCanary = "PUBLICATION_URL_PRIVATE_CANARY"
	_, targetErr := publicationdns.Target("https://operator:"+privateCanary+"@bad host.example/preview", "")
	if targetErr == nil {
		t.Fatal("malformed publication endpoint was accepted")
	}
	request, logs, reference := diagnosticRequest("/v1/tunnels")
	response := httptest.NewRecorder()
	writeTunnelError(response, request, errors.Join(targetErr, workspace.ErrForbidden, syscall.EIO))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("mixed workspace/operational failure status=%d, want 500: %s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "internal_error" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" || body.Error.Outcome != "uncertain" || body.Error.Retryable == nil || !*body.Error.Retryable || body.Error.RepairAction != "retry" {
		t.Fatalf("safe retry response lost failure identity: %+v", body.Error)
	}
	if strings.Contains(response.Body.String()+logs.String(), privateCanary) {
		t.Fatal("publication endpoint credentials reached the response or logs")
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("want exactly one structured failure event: %v; logs=%q", err, logs.String())
	}
	for key, want := range map[string]any{
		"stage":             "http_handler",
		"code":              "internal_error",
		"support_reference": reference,
		"correlation_id":    "correlation_fault_01",
		"errno":             float64(syscall.EIO),
		"http_status":       float64(http.StatusInternalServerError),
	} {
		if event[key] != want {
			t.Errorf("fault event %s=%v, want %v: %s", key, event[key], want, logs.String())
		}
	}
	chain, ok := event["error_chain"].([]any)
	if !ok {
		t.Fatalf("fault event omitted bounded error types: %#v", event["error_chain"])
	}
	foundURLCause := false
	for _, value := range chain {
		if name, ok := value.(string); ok && strings.Contains(name, "url.Error") {
			foundURLCause = true
		}
	}
	if !foundURLCause {
		t.Fatalf("typed URL parsing cause was not preserved in error_chain: %v", chain)
	}
}

func TestTunnelErrorMixedDomainFailuresReachFaultOwner(t *testing.T) {
	const privateCanary = "TUNNEL_OPERATION_PRIVATE_CANARY"
	tests := []struct {
		name string
		err  error
	}{
		{name: "generation", err: tunnelv1.ErrGenerationConflict},
		{name: "name conflict", err: tunnelv1.ErrNameConflict},
		{name: "authorization", err: previewtunnelapi.ErrForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/tunnels/tun_fault_01")
			response := httptest.NewRecorder()
			writeTunnelError(response, request, errors.Join(test.err, syscall.EIO, errors.New(privateCanary)))
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("mixed typed/operational failure status=%d, want 500: %s", response.Code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "internal_error" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" || body.Error.Outcome != "uncertain" || body.Error.Retryable == nil || !*body.Error.Retryable || body.Error.RepairAction != "retry" {
				t.Fatalf("safe response lost failure identity/recovery: %+v", body.Error)
			}
			if strings.Contains(response.Body.String()+logs.String(), privateCanary) {
				t.Fatal("private operation text reached the response or logs")
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("want exactly one structured failure event: %v; logs=%q", err, logs.String())
			}
			for key, want := range map[string]any{
				"stage":             "http_handler",
				"code":              "internal_error",
				"support_reference": reference,
				"correlation_id":    "correlation_fault_01",
				"errno":             float64(syscall.EIO),
				"http_status":       float64(http.StatusInternalServerError),
			} {
				if event[key] != want {
					t.Errorf("fault event %s=%v, want %v: %s", key, event[key], want, logs.String())
				}
			}
		})
	}
}
