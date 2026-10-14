package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

func TestUserMachinePersistenceFailuresReturnSafe503(t *testing.T) {
	store, err := db.Open(config.Database{
		Driver: "pgx",
		DSN:    "postgres://paperboat:paperboat@127.0.0.1:1/paperboat_test?sslmode=disable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{}, nil)

	for _, tc := range []struct {
		name       string
		path       string
		param      string
		body       string
		withClient bool
		wantCode   string
		handler    func() http.HandlerFunc
	}{
		{
			name:    "enrollment status",
			path:    "/v1/machines/enrollments/enrollment_fault_01",
			param:   "enrollment_id",
			handler: func() http.HandlerFunc { return userMachineEnrollmentStatus(service) },
		},
		{
			name:    "enrollment cancellation",
			path:    "/v1/machines/enrollments/enrollment_fault_01/cancel",
			param:   "enrollment_id",
			handler: func() http.HandlerFunc { return userMachineEnrollmentCancel(service) },
		},
		{
			name:    "machine disconnect",
			path:    "/v1/machines/machine_fault_01/disconnect",
			param:   "machine_id",
			handler: func() http.HandlerFunc { return userMachineDisconnect(service) },
		},
		{
			name:       "machine connection source lookup",
			path:       "/v1/machines/machine_fault_01/connect",
			param:      "machine_id",
			body:       `{"source_machine_id":"source_fault_01"}`,
			withClient: true,
			wantCode:   "connector_unavailable",
			handler:    func() http.HandlerFunc { return userMachineConnectionDescriptor(service) },
		},
		{
			name:       "shared terminal source lookup",
			path:       "/v1/terminal-sessions/session_fault_01/connect",
			param:      "session_id",
			body:       `{"source_machine_id":"source_fault_01"}`,
			withClient: true,
			wantCode:   "connector_unavailable",
			handler:    func() http.HandlerFunc { return sharedTerminalConnectionDescriptor(service) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest(tc.path)
			client := (*auth.ClientPrincipal)(nil)
			if tc.withClient {
				client = &auth.ClientPrincipal{SessionID: "cli_fault_01"}
				request.Method = http.MethodPost
				request.Body = io.NopCloser(strings.NewReader(tc.body))
			}
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{
				User: auth.User{ID: "account_fault_01"}, Client: client,
			}))
			request.SetPathValue(tc.param, "resource_fault_01")
			response := httptest.NewRecorder()
			tc.handler().ServeHTTP(response, request)

			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantCode := tc.wantCode
			if wantCode == "" {
				wantCode = "temporarily_unavailable"
			}
			if body.Error.Code != wantCode || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
				t.Fatalf("error response omitted safe failure identity: %+v", body.Error)
			}
			if strings.Contains(response.Body.String(), "source_fault_01") || strings.Contains(logs.String(), "source_fault_01") {
				t.Fatal("source machine reference leaked into the response or diagnostic log")
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("decode structured failure log: %v (%s)", err, logs.String())
			}
			for key, want := range map[string]string{
				"component":         "paperboat-server",
				"stage":             "http_handler",
				"code":              wantCode,
				"support_reference": reference,
				"correlation_id":    "correlation_fault_01",
				"outcome":           "failed",
			} {
				if event[key] != want {
					t.Errorf("log %s=%v, want %q; log=%s", key, event[key], want, logs.String())
				}
			}
			if event["error_type"] == "" || event["cause"] == "" {
				t.Errorf("log omitted bounded cause metadata: %s", logs.String())
			}
		})
	}
}
