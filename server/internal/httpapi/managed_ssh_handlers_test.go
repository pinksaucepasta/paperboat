package httpapi

import (
	"encoding/json"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/managedssh"
)

func TestManagedSSHErrorPreservesPureResponsesAndReportsOperationalFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		code   string
	}{
		{"invalid", wire.ErrInvalid, 400, "invalid_request"},
		{"conflict", managedssh.ErrConflict, 409, "operation_conflict"},
		{"capacity", managedssh.ErrMachineKeyCapacity, 409, "managed_ssh_key_capacity"},
		{"unavailable", managedssh.ErrUnavailable, 404, "not_found"},
		{"native IO", syscall.EIO, 503, "managed_ssh_unavailable"},
		{"mixed conflict", errors.Join(managedssh.ErrConflict, syscall.EIO), 503, "managed_ssh_unavailable"},
		{"mixed capacity", errors.Join(managedssh.ErrMachineKeyCapacity, syscall.EIO), 503, "managed_ssh_unavailable"},
		{"mixed unavailable", errors.Join(managedssh.ErrUnavailable, syscall.EIO), 503, "managed_ssh_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, logs, ref := diagnosticRequest("/v1/managed-ssh/targets")
			response := httptest.NewRecorder()
			if !managedSSHError(response, req, tc.cause) || response.Code != tc.status {
				t.Fatalf("status=%d want%d", response.Code, tc.status)
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("response must contain exactly one error: %v", err)
			}
			if body.Error.Code != tc.code {
				t.Fatalf("code=%s want%s", body.Error.Code, tc.code)
			}
			if tc.status < 500 {
				if logs.Len() != 0 {
					t.Fatal("pure rejection reported as fault")
				}
				return
			}
			if body.Error.SupportReference != ref {
				t.Fatal("response omitted fault reference")
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("want one fault log: %v", err)
			}
			if event["errno"] != float64(syscall.EIO) || event["support_reference"] != ref {
				t.Fatal("original IO or correlation lost")
			}
		})
	}
	if managedSSHError(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/managed-ssh/targets", nil), nil) {
		t.Fatal("nil error handled as failure")
	}
}
