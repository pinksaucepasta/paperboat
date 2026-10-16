package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestWorkOSCallbackFailureSeparatesRejectionProviderAndStoreErrors(t *testing.T) {
	storeErr := &pgconn.PgError{Code: "40001", Message: "SECRET_DATABASE_DETAIL"}
	for _, tc := range []struct {
		name        string
		err         error
		status      int
		code        string
		cause       string
		wantCapture bool
	}{
		{name: "plus address", err: emailpolicy.ErrPlusAddress, status: 400, code: "email_alias_not_allowed"},
		{name: "duplicate email", err: emailpolicy.ErrDuplicate, status: 409, code: "email_already_registered"},
		{name: "invalid email", err: emailpolicy.ErrInvalid, status: 400, code: "invalid_email"},
		{name: "invalid code", err: &auth.WorkOSVerificationError{Reason: "provider_http_error", HTTPStatus: http.StatusBadRequest}, status: http.StatusUnauthorized, code: "auth_callback_failed"},
		{name: "provider transport failure", err: &auth.WorkOSVerificationError{Reason: "provider_transport", HTTPStatus: http.StatusServiceUnavailable, Cause: syscall.ETIMEDOUT}, status: http.StatusServiceUnavailable, code: "provider_unavailable", cause: "network_timeout", wantCapture: true},
		{name: "database failure", err: errors.Join(errors.New("upsert user and session"), storeErr), status: http.StatusInternalServerError, code: "auth_callback_failed", cause: "postgres_serialization", wantCapture: true},
		{name: "impersonation policy rejection", err: auth.ErrWorkOSImpersonation, status: http.StatusUnauthorized, code: "auth_callback_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &supportTestStore{}
			reports, err := supportreport.New(store, supportreport.Config{})
			if err != nil {
				t.Fatal(err)
			}
			runCtx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { _ = reports.Run(runCtx); close(done) }()
			var logs strings.Builder
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			handler := supportMiddleware(reports, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeWorkOSCallbackFailure(w, r, tc.err)
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/workos/callback", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("callback error = %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "SECRET_") {
				t.Fatal("callback response disclosed provider or database details")
			}

			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("support reporter did not drain")
			}
			store.mu.Lock()
			records := append([]supportreport.Record(nil), store.records...)
			store.mu.Unlock()
			if !tc.wantCapture {
				if len(records) != 0 {
					t.Fatalf("expected rejection created support failure: %+v", records)
				}
				if !strings.Contains(logs.String(), "support_reference") || !strings.Contains(logs.String(), "outcome=rejected") {
					t.Fatalf("expected rejection lacks correlated safe warning: %s", logs.String())
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("stored records = %d, want one failure: %+v", len(records), records)
			}
			record := records[0]
			if record.Code != tc.code || record.Cause != tc.cause || record.HTTPStatus != tc.status || record.ErrorType == "" || len(record.ErrorChain) == 0 {
				t.Fatalf("callback failure cause not captured: %+v", record)
			}
			if strings.Contains(strings.Join(record.ErrorChain, " "), "SECRET_") || strings.Contains(logs.String(), "SECRET_") {
				t.Fatal("callback logs or evidence disclosed error text")
			}
		})
	}
}

func TestCSRFValidationFailureSeparatesRejectionFromDependencyError(t *testing.T) {
	invalidResponse := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	writeCSRFValidationError(invalidResponse, invalidRequest, auth.ErrCSRF)
	if invalidResponse.Code != http.StatusForbidden || !strings.Contains(invalidResponse.Body.String(), `"code":"csrf_failed"`) {
		t.Fatalf("invalid CSRF token = %d %s", invalidResponse.Code, invalidResponse.Body.String())
	}

	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := supportMiddleware(reports, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCSRFValidationError(w, r, errors.New("SECRET_DATABASE_DETAIL"))
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"authorization_unavailable"`) {
		t.Fatalf("CSRF dependency failure = %d %s", response.Code, response.Body.String())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("support reporter did not drain")
	}
	store.mu.Lock()
	records := append([]supportreport.Record(nil), store.records...)
	store.mu.Unlock()
	if len(records) != 1 || records[0].Cause != "service_unavailable" || records[0].HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("CSRF dependency failure was not captured: %+v", records)
	}
	if strings.Contains(response.Body.String(), "SECRET_DATABASE_DETAIL") || strings.Contains(logs.String(), "SECRET_DATABASE_DETAIL") {
		t.Fatal("CSRF dependency error text leaked")
	}
}
