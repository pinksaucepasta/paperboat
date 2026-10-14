package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type pendingEndpointReaderFunc func(context.Context, string, time.Time) ([]peeridentity.EndpointEnrollmentRequest, error)

func (f pendingEndpointReaderFunc) PendingEndpoints(ctx context.Context, account string, now time.Time) ([]peeridentity.EndpointEnrollmentRequest, error) {
	return f(ctx, account, now)
}

func diagnosticRequest(path string) (*http.Request, *bytes.Buffer, string) {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reference := supportreport.NewReference()
	ctx := serverdiagnostics.WithRequestID(request.Context(), "request_fault_01")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "correlation_fault_01")
	ctx = context.WithValue(ctx, supportContextKey{}, supportContext{reference: reference, logger: logger})
	return request.WithContext(ctx), &logs, reference
}

func TestPendingEndpointsLogsActualFailureCauseSafely(t *testing.T) {
	request, logs, reference := diagnosticRequest("/v1/e2ee/pending-endpoints")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{
		User:   auth.User{ID: "account_fault_01"},
		Client: &auth.ClientPrincipal{SessionID: "cli_fault_01"},
	}))
	response := httptest.NewRecorder()
	err := errors.Join(syscall.ECONNREFUSED, errors.New("DB_SECRET_CANARY"))
	pendingEndpoints(pendingEndpointReaderFunc(func(context.Context, string, time.Time) ([]peeridentity.EndpointEnrollmentRequest, error) {
		return nil, err
	})).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, value := range []string{"connection_refused", "http_network", "temporarily_unavailable", reference, "correlation_fault_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("safe failure log omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(logs.String(), "DB_SECRET_CANARY") || strings.Contains(response.Body.String(), "DB_SECRET_CANARY") {
		t.Fatal("pending endpoint failure exposed arbitrary error text")
	}
}

func TestViewerPolicyFailureLogsActualCauseSafely(t *testing.T) {
	request, logs, reference := diagnosticRequest("/v1/tunnels/tunnel_fault_01/viewer-policy")
	response := httptest.NewRecorder()
	err := errors.Join(syscall.ECONNREFUSED, errors.New("POLICY_DB_SECRET"))
	browserViewerPolicyError(response, request, err)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, value := range []string{"connection_refused", "http_tunnels", "viewer_policy_unavailable", reference, "correlation_fault_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("safe failure log omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(logs.String(), "POLICY_DB_SECRET") || strings.Contains(response.Body.String(), "POLICY_DB_SECRET") {
		t.Fatal("viewer policy failure exposed arbitrary error text")
	}
}
