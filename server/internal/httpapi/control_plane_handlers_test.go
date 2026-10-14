package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestAdminUsageMutationErrorsKeepValidationAndCaptureUnexpectedCause(t *testing.T) {
	invalidResponse := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodPost, "/v1/admin/edge/usage-keys", nil)
	writeAdminUsageKeyMutationError(invalidResponse, invalidRequest, controlplane.ErrInvalidUsageReport,
		"invalid key", "conflicting key", "unavailable")
	if invalidResponse.Code != http.StatusBadRequest || !strings.Contains(invalidResponse.Body.String(), `"code":"validation_failed"`) {
		t.Fatalf("typed validation error = %d %s", invalidResponse.Code, invalidResponse.Body.String())
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
		writeAdminUsageKeyMutationError(w, r, errors.New("SECRET_DATABASE_DETAIL"),
			"invalid key", "conflicting key", "operation unavailable")
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/edge/usage-keys", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"control_unavailable"`) {
		t.Fatalf("unexpected mutation error = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "SECRET_DATABASE_DETAIL") {
		t.Fatal("HTTP response disclosed the internal cause")
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
	if len(records) != 1 {
		t.Fatalf("stored records = %d, want one failure", len(records))
	}
	record := records[0]
	if record.Code != "control_unavailable" || record.HTTPStatus != http.StatusServiceUnavailable || record.Cause != "service_unavailable" || len(record.ErrorChain) == 0 {
		t.Fatalf("failure cause was not retained safely: %+v", record)
	}
	if strings.Contains(strings.Join(record.ErrorChain, " "), "SECRET_DATABASE_DETAIL") || strings.Contains(logs.String(), "SECRET_DATABASE_DETAIL") {
		t.Fatal("support evidence or local log disclosed the internal cause text")
	}
}

func TestAdminUsageMutationForbiddenAndConflictRemainExpected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
		body string
	}{
		{name: "forbidden", err: controlplane.ErrAdminMutationForbidden, code: http.StatusForbidden, body: `"code":"forbidden"`},
		{name: "conflict", err: controlplane.ErrUsageOperationConflict, code: http.StatusConflict, body: `"code":"idempotency_key_conflict"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/admin/edge/usage-keys", nil)
			writeAdminUsageKeyMutationError(response, request, tc.err, "invalid", "conflict", "unavailable")
			if response.Code != tc.code || !strings.Contains(response.Body.String(), tc.body) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRouteIntentUnexpectedStoreFailureKeepsSafeCause(t *testing.T) {
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
		writeRouteIntentError(w, r, errors.New("SECRET_ROUTE_DATABASE_DETAIL"))
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/environments/env-private/routes", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"control_unavailable"`) {
		t.Fatalf("unexpected route-store failure = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "SECRET_ROUTE_DATABASE_DETAIL") {
		t.Fatal("HTTP response disclosed the internal cause")
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
	if len(records) != 1 {
		t.Fatalf("stored records = %d, want one failure", len(records))
	}
	record := records[0]
	if record.Code != "control_unavailable" || record.HTTPStatus != http.StatusServiceUnavailable || record.Cause != "service_unavailable" || len(record.ErrorChain) == 0 {
		t.Fatalf("route failure cause was not retained safely: %+v", record)
	}
	if strings.Contains(strings.Join(record.ErrorChain, " "), "SECRET_ROUTE_DATABASE_DETAIL") || strings.Contains(logs.String(), "SECRET_ROUTE_DATABASE_DETAIL") {
		t.Fatal("support evidence or local log disclosed the internal cause text")
	}
}
