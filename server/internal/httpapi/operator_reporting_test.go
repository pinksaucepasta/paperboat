package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorreporting"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

func TestOperatorDiagnosticsStatusRequiresPureExpectedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		kind string
	}{
		{name: "wrapped invalid input", err: fmt.Errorf("request: %w", operatorreporting.ErrInvalid), code: http.StatusBadRequest, kind: "invalid_request"},
		{name: "pure denial", err: operatorreporting.ErrForbidden, code: http.StatusForbidden, kind: "forbidden"},
		{name: "repeated pure denial", err: errors.Join(operatorreporting.ErrForbidden, operatorreporting.ErrForbidden), code: http.StatusForbidden, kind: "forbidden"},
		{name: "denial with database failure", err: errors.Join(operatorreporting.ErrForbidden, syscall.EIO), code: http.StatusServiceUnavailable, kind: "operator_evidence_unavailable"},
		{name: "conflict with storage failure", err: errors.Join(backupreport.ErrConflict, syscall.EIO), code: http.StatusServiceUnavailable, kind: "operator_evidence_unavailable"},
		{name: "rate limit with transport failure", err: errors.Join(operatorreporting.ErrRateLimited, syscall.ECONNRESET), code: http.StatusServiceUnavailable, kind: "operator_evidence_unavailable"},
		{name: "pure unsupported capability", err: operatorreporting.ErrUnsupported, code: http.StatusNotImplemented, kind: "sentry_capability_unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			operatorDiagnosticsError(response, httptest.NewRequest(http.MethodGet, "/v1/admin/sentry/issues", nil), test.err)
			if response.Code != test.code {
				t.Fatalf("status=%d, want %d; body=%s", response.Code, test.code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.kind {
				t.Fatalf("error code=%q, want %q", body.Error.Code, test.kind)
			}
			if strings.Contains(response.Body.String(), "input/output error") || strings.Contains(response.Body.String(), "connection reset") {
				t.Fatal("operational cause leaked into public response")
			}
		})
	}
}

func TestBackupReportingCredentialCannotBecomeStaff(t *testing.T) {
	token := strings.Repeat("producer-key", 4)
	service, err := backupreport.NewService(nil, backupreport.Config{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupReportRoute(mux, Options{OperatorBackups: service})
	for _, headers := range [][]string{nil, {"Bearer wrong"}, {"Bearer " + token, "Bearer " + token}} {
		request := httptest.NewRequest("POST", "/v1/operations/backups/reports", strings.NewReader(`{"password":"never-read"}`))
		for _, h := range headers {
			request.Header.Add("Authorization", h)
		}
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, request)
		if out.Code != 401 || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("producer authority accepted", out.Code)
		}
	}
	request := httptest.NewRequest("POST", "/v1/operations/backups/reports", strings.NewReader(strings.Repeat("x", 8193)))
	request.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, request)
	if out.Code < 400 {
		t.Fatal("oversized body accepted")
	}
}
