package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
)

type fakePreviewTunnelAPI struct {
	operation previewtunnelapi.Operation
	events    previewtunnelapi.EventPage
	err       error
	request   previewtunnelapi.RequestContext
	cancel    context.CancelFunc
}

func (f *fakePreviewTunnelAPI) GetOperation(_ context.Context, request previewtunnelapi.RequestContext, _ string) (previewtunnelapi.Operation, error) {
	f.request = request
	return f.operation, f.err
}

func (f *fakePreviewTunnelAPI) CancelOperation(_ context.Context, request previewtunnelapi.RequestContext, _ string) (previewtunnelapi.Operation, error) {
	f.request = request
	return f.operation, f.err
}

func (f *fakePreviewTunnelAPI) ListEvents(_ context.Context, request previewtunnelapi.RequestContext, _, _, _ string, _ int) (previewtunnelapi.EventPage, error) {
	f.request = request
	if f.cancel != nil {
		f.cancel()
	}
	return f.events, f.err
}

func TestPreviewTunnelOperationHandlerPropagatesIdentity(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC)
	service := &fakePreviewTunnelAPI{operation: previewtunnelapi.Operation{
		Schema: "paperboat.preview-tunnel/v1", Kind: "operation", ID: "op_1", ResourceKind: "tunnel",
		ResourceID: "tun_1", Phase: "ready", State: "succeeded", Progress: 100,
		CorrelationID: "corr_1", CreatedAt: now, UpdatedAt: now,
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/operations/op_1", nil)
	req.SetPathValue("operation_id", "op_1")
	client := auth.ClientPrincipal{SessionID: "machine_1", Scopes: []string{"operations:read"}}
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{
		User: auth.User{ID: "acct_1", Role: auth.RoleUser}, Client: &client,
	})
	ctx = serverdiagnostics.WithRequestID(ctx, "req_1")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_1")
	rec := httptest.NewRecorder()
	previewTunnelOperationGet(service).ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if service.request.Actor.AccountID != "acct_1" || service.request.Actor.MachineID != "machine_1" || service.request.CorrelationID != "corr_1" {
		t.Fatalf("request context = %#v", service.request)
	}
}

func TestPreviewTunnelOperationHandlerReturnsTypedConflict(t *testing.T) {
	service := &fakePreviewTunnelAPI{err: previewtunnelapi.ErrOperationNotCancellable}
	req := httptest.NewRequest(http.MethodDelete, "/v1/operations/op_1", nil)
	req.SetPathValue("operation_id", "op_1")
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1", Role: auth.RoleUser}})
	ctx = serverdiagnostics.WithRequestID(ctx, "req_1")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_1")
	rec := httptest.NewRecorder()
	previewTunnelOperationCancel(service).ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "operation_not_cancellable" || response.Error.Outcome != "uncertain" || response.Error.CorrelationID != "corr_1" || response.Error.Retryable == nil || *response.Error.Retryable {
		t.Fatalf("error = %#v", response.Error)
	}
}

func TestPreviewTunnelServiceErrorPreservesPureExpectedStatuses(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "not found", err: previewtunnelstore.ErrNotFound, status: http.StatusNotFound, code: "operation_not_found"},
		{name: "forbidden", err: previewtunnelapi.ErrForbidden, status: http.StatusForbidden, code: "forbidden"},
		{name: "host actor required", err: previewtunnelapi.ErrHostActorRequired, status: http.StatusForbidden, code: "forbidden"},
		{name: "not cancellable", err: previewtunnelapi.ErrOperationNotCancellable, status: http.StatusConflict, code: "operation_not_cancellable"},
		{name: "invalid cursor", err: previewtunnelapi.ErrInvalidCursor, status: http.StatusBadRequest, code: "invalid_cursor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, logs, _ := diagnosticRequest("/v1/operations/op_1")
			response := httptest.NewRecorder()
			writePreviewTunnelServiceError(response, req, test.err)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.status)
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code {
				t.Fatalf("error code=%q, want %q", body.Error.Code, test.code)
			}
			if logs.Len() != 0 {
				t.Fatalf("pure expected status emitted unexpected failure log: %s", logs.String())
			}
		})
	}
}

func TestPreviewTunnelMixedCancellationFailureUsesOneSafeFinalOwner(t *testing.T) {
	req, logs, reference := diagnosticRequest("/v1/operations/op_1")
	req.SetPathValue("operation_id", "op_1")
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1", Role: auth.RoleUser}})
	req = req.WithContext(ctx)
	private := errors.New("PREVIEW_TUNNEL_PRIVATE_ROLLBACK")
	service := &fakePreviewTunnelAPI{err: errors.Join(previewtunnelapi.ErrOperationNotCancellable, syscall.EIO, private)}
	response := httptest.NewRecorder()
	previewTunnelOperationCancel(service).ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("mixed cancellation/storage failure became a non-5xx status: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), private.Error()) {
		t.Fatal("HTTP response exposed the private cleanup cause")
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "internal_error" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("failure response lost safe code or correlation: %+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one JSON failure record: %v; logs=%s", err, logs.String())
	}
	if event["stage"] != "http_handler" || event["code"] != "internal_error" || event["cause"] != "system_error" || event["errno"] != float64(syscall.EIO) || event["http_status"] != float64(http.StatusInternalServerError) || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["request_id"] != "request_fault_01" {
		t.Fatalf("final failure record lost original cause or request identity: %v", event)
	}
	if strings.Contains(logs.String(), private.Error()) {
		t.Fatal("failure log exposed the private cleanup cause")
	}
}

func TestPreviewTunnelEventErrorPreservesPureExpectedResponses(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		status       int
		code         string
		retryable    bool
		repairAction string
	}{
		{name: "forbidden", err: previewtunnelapi.ErrForbidden, status: http.StatusForbidden, code: "forbidden", repairAction: "authenticate_with_required_scope"},
		{name: "host actor required", err: previewtunnelapi.ErrHostActorRequired, status: http.StatusForbidden, code: "forbidden", repairAction: "authenticate_with_required_scope"},
		{name: "invalid cursor", err: previewtunnelapi.ErrInvalidCursor, status: http.StatusBadRequest, code: "invalid_cursor", repairAction: "restart_event_stream"},
		{name: "unsafe metadata", err: previewtunnelapi.ErrUnsafeMetadata, status: http.StatusInternalServerError, code: "unsafe_event_metadata", repairAction: "contact_support"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, logs, _ := diagnosticRequest("/v1/previews/prv_1/events")
			response := httptest.NewRecorder()
			writePreviewTunnelEventError(response, req, test.err)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.status)
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code || body.Error.RepairAction != test.repairAction || body.Error.Retryable == nil || *body.Error.Retryable != test.retryable {
				t.Fatalf("error response changed: %+v", body.Error)
			}
			if test.status < 500 && logs.Len() != 0 {
				t.Fatalf("pure expected rejection emitted failure log: %s", logs.String())
			}
		})
	}
}

func TestPreviewTunnelMixedEventFailuresUseOneSafeFinalOwner(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "forbidden", err: errors.Join(previewtunnelapi.ErrForbidden, syscall.EIO)},
		{name: "host actor required", err: errors.Join(previewtunnelapi.ErrHostActorRequired, syscall.EIO)},
		{name: "invalid cursor", err: errors.Join(previewtunnelapi.ErrInvalidCursor, syscall.EIO)},
		{name: "unsafe metadata", err: errors.Join(previewtunnelapi.ErrUnsafeMetadata, syscall.EIO)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, logs, reference := diagnosticRequest("/v1/previews/prv_1/events")
			private := errors.New("PREVIEW_EVENT_PRIVATE_STORAGE_DETAIL")
			err := errors.Join(test.err, private)
			response := httptest.NewRecorder()
			writePreviewTunnelEventError(response, req, err)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("mixed expected/storage failure status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), private.Error()) {
				t.Fatal("HTTP response exposed private storage detail")
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "internal_error" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
				t.Fatalf("failure response lost safe code or correlation: %+v", body.Error)
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("expected exactly one JSON failure record: %v; logs=%s", err, logs.String())
			}
			if event["stage"] != "http_handler" || event["code"] != "internal_error" || event["cause"] != "system_error" || event["errno"] != float64(syscall.EIO) || event["http_status"] != float64(http.StatusInternalServerError) || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["request_id"] != "request_fault_01" {
				t.Fatalf("final failure record lost original cause or request identity: %v", event)
			}
			if strings.Contains(logs.String(), private.Error()) {
				t.Fatal("failure log exposed private storage detail")
			}
		})
	}
}

func TestPreviewTunnelScopeAndCorrelationMiddleware(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if serverdiagnostics.CorrelationID(r.Context()) != "corr_1" {
			t.Fatalf("correlation ID = %q", serverdiagnostics.CorrelationID(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	client := auth.ClientPrincipal{Scopes: []string{"operations:read"}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Correlation-Id", "corr_1")
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}, Client: &client})
	rec := httptest.NewRecorder()
	requestID(correlationID(requireScope("operations:write", next))).ServeHTTP(rec, req.WithContext(ctx))
	if called || rec.Code != http.StatusForbidden {
		t.Fatalf("called = %v, status = %d", called, rec.Code)
	}

	client.Scopes = []string{"operations:write"}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Correlation-Id", "corr_1")
	ctx = context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}, Client: &client})
	rec = httptest.NewRecorder()
	requestID(correlationID(requireScope("operations:write", next))).ServeHTTP(rec, req.WithContext(ctx))
	if !called || rec.Code != http.StatusNoContent || rec.Header().Get("Correlation-Id") != "corr_1" {
		t.Fatalf("called = %v, status = %d, correlation = %q", called, rec.Code, rec.Header().Get("Correlation-Id"))
	}
}

func TestPreviewTunnelServiceErrorDoesNotExposeCause(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := serverdiagnostics.WithRequestID(req.Context(), "req_1")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_1")
	rec := httptest.NewRecorder()
	writePreviewTunnelServiceError(rec, req.WithContext(ctx), errors.New("Bearer should-never-leak"))
	if rec.Code != http.StatusInternalServerError || rec.Body.String() == "" {
		t.Fatalf("status = %d", rec.Code)
	}
	if json.Valid(rec.Body.Bytes()) == false {
		t.Fatalf("invalid JSON: %s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("should-never-leak")) {
		t.Fatalf("cause leaked: %s", rec.Body.String())
	}
}

func TestPreviewTunnelEventsReturnsJSONPage(t *testing.T) {
	service := &fakePreviewTunnelAPI{events: previewtunnelapi.EventPage{Items: []previewtunnelapi.Event{{
		Schema: "paperboat.preview-tunnel/v1", Kind: "event", ID: "evt_1", Cursor: "cur_1",
		EventType: "preview.created", ResourceKind: "preview_lease", ResourceID: "prv_1",
		OccurredAt: time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC),
		Actor:      previewtunnelapi.EventActor{Type: "user", ID: "acct_1"}, CorrelationID: "corr_1",
		SafeMetadata: map[string]any{"generation": 1},
	}}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/previews/prv_1/events?limit=10", nil)
	req.SetPathValue("preview_id", "prv_1")
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1", Role: auth.RoleUser}})
	rec := httptest.NewRecorder()
	previewTunnelEvents(service, "preview_lease", "preview_id").ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"event_type":"preview.created"`)) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPreviewTunnelEventsStreamsAndResumes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := &fakePreviewTunnelAPI{cancel: cancel, events: previewtunnelapi.EventPage{Items: []previewtunnelapi.Event{{
		Schema: "paperboat.preview-tunnel/v1", Kind: "event", ID: "evt_1", Cursor: "cur_2",
		EventType: "tunnel.paused", ResourceKind: "tunnel", ResourceID: "tun_1",
		OccurredAt: time.Date(2026, 8, 30, 1, 2, 3, 0, time.UTC),
		Actor:      previewtunnelapi.EventActor{Type: "host", ID: "host_1"}, CorrelationID: "corr_1",
		SafeMetadata: map[string]any{"generation": 2},
	}}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/tunnels/tun_1/events?cursor=cur_1", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "cur_1")
	req.SetPathValue("tunnel_id", "tun_1")
	authCtx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1", Role: auth.RoleUser}})
	rec := httptest.NewRecorder()
	previewTunnelEvents(service, "tunnel", "tunnel_id").ServeHTTP(rec, req.WithContext(authCtx))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" || !rec.Flushed {
		t.Fatalf("status = %d, content-type = %q, flushed = %v", rec.Code, rec.Header().Get("Content-Type"), rec.Flushed)
	}
	if body := rec.Body.String(); !strings.Contains(body, "id: cur_2\n") || !strings.Contains(body, `"event_type":"tunnel.paused"`) {
		t.Fatalf("stream body = %q", body)
	}
}

func TestPreviewTunnelEventsRejectsConflictingResumeCursors(t *testing.T) {
	service := &fakePreviewTunnelAPI{}
	req := httptest.NewRequest(http.MethodGet, "/v1/tunnels/tun_1/events?cursor=cur_1", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "cur_other")
	req.SetPathValue("tunnel_id", "tun_1")
	ctx := context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1", Role: auth.RoleUser}})
	rec := httptest.NewRecorder()
	previewTunnelEvents(service, "tunnel", "tunnel_id").ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"invalid_cursor"`)) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
