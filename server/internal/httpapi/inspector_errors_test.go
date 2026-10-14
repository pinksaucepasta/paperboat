package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/inspectaccess"
)

func TestInspectorErrorKeepsPureStatusesAndReportsMixedFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid", err: fmt.Errorf("wrapped: %w", inspectaccess.ErrInvalid), status: http.StatusBadRequest},
		{name: "denied", err: fmt.Errorf("wrapped: %w", inspectaccess.ErrNotAuthorized), status: http.StatusForbidden},
		{name: "capacity", err: fmt.Errorf("wrapped: %w", inspectaccess.ErrCapacity), status: http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			inspectorError(response, httptest.NewRequest(http.MethodPost, "/v1/inspector", nil), tc.err)
			if response.Code != tc.status {
				t.Fatalf("pure expected result status=%d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
		})
	}

	const privateCanary = "INSPECTOR_TOKEN_PRIVATE_CANARY"
	for _, expected := range []error{inspectaccess.ErrInvalid, inspectaccess.ErrNotAuthorized, inspectaccess.ErrCapacity} {
		t.Run(expected.Error(), func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/inspector")
			response := httptest.NewRecorder()
			cause := errors.Join(expected, syscall.EIO, errors.New(privateCanary))
			inspectorError(response, request, cause)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("mixed expected/operational error status=%d, want 500: %s", response.Code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "inspector_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
				t.Fatalf("safe failure identity missing: %+v", body.Error)
			}
			if strings.Contains(response.Body.String()+logs.String(), privateCanary) {
				t.Fatalf("private credential text reached response or logs: %s", response.Body.String()+logs.String())
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("want exactly one structured failure event: %v; logs=%q", err, logs.String())
			}
			for key, want := range map[string]any{
				"stage":             "http_handler",
				"code":              "inspector_unavailable",
				"support_reference": reference,
				"correlation_id":    "correlation_fault_01",
				"errno":             float64(syscall.EIO),
			} {
				if event[key] != want {
					t.Errorf("safe failure event %s=%v, want %v: %s", key, event[key], want, logs.String())
				}
			}
		})
	}
}
