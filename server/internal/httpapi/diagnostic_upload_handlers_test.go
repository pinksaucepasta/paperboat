package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/diagnosticuploads"
)

type diagnosticRepositoryStub struct {
	intent      diagnosticuploads.Intent
	reserveErr  error
	getErr      error
	completeErr error
}

func (s *diagnosticRepositoryStub) Reserve(_ context.Context, request diagnosticuploads.CreateRequest, hash [32]byte, proposed diagnosticuploads.Intent) (diagnosticuploads.Intent, error) {
	if s.reserveErr != nil {
		return diagnosticuploads.Intent{}, s.reserveErr
	}
	if request.UserID != "usr_test" || request.CLIClientSessionID != "cli_test" {
		return diagnosticuploads.Intent{}, diagnosticuploads.ErrNotFound
	}
	if s.intent.ID == "" {
		s.intent = proposed
	}
	if s.intent.RequestHash != hash {
		return diagnosticuploads.Intent{}, diagnosticuploads.ErrIdempotencyConflict
	}
	return s.intent, nil
}
func (s *diagnosticRepositoryStub) Get(_ context.Context, userID, intentID string) (diagnosticuploads.Intent, error) {
	if s.getErr != nil {
		return diagnosticuploads.Intent{}, s.getErr
	}
	if userID != s.intent.UserID || intentID != s.intent.ID {
		return diagnosticuploads.Intent{}, diagnosticuploads.ErrNotFound
	}
	return s.intent, nil
}
func (s *diagnosticRepositoryStub) Complete(_ context.Context, userID, intentID string, metadata diagnosticuploads.ObjectMetadata, now time.Time) (diagnosticuploads.Intent, error) {
	if s.completeErr != nil {
		return diagnosticuploads.Intent{}, s.completeErr
	}
	if userID != s.intent.UserID || intentID != s.intent.ID || metadata.Bytes != s.intent.Bytes || metadata.SHA256 != s.intent.SHA256 {
		return diagnosticuploads.Intent{}, diagnosticuploads.ErrUploadMismatch
	}
	s.intent.State, s.intent.UploadedAt, s.intent.ObjectETag = "uploaded", now, metadata.ETag
	return s.intent, nil
}

type diagnosticObjectsStub struct {
	metadata     diagnosticuploads.ObjectMetadata
	authority    diagnosticuploads.UploadAuthority
	authorizeErr error
	statErr      error
}

func (s *diagnosticObjectsStub) AuthorizePut(_ context.Context, _ string, _ int64, _ [32]byte, _ time.Time) (diagnosticuploads.UploadAuthority, error) {
	if s.authorizeErr != nil {
		return diagnosticuploads.UploadAuthority{}, s.authorizeErr
	}
	if s.authority.URL != "" {
		return s.authority, nil
	}
	return diagnosticuploads.UploadAuthority{URL: "https://objects.example.test/upload", Headers: map[string]string{"X-Amz-Checksum-Sha256": "checksum", "If-None-Match": "*"}}, nil
}
func (s *diagnosticObjectsStub) Stat(_ context.Context, _ string) (diagnosticuploads.ObjectMetadata, error) {
	return s.metadata, s.statErr
}

func TestDiagnosticUploadHandlerLifecycle(t *testing.T) {
	repository, objects := &diagnosticRepositoryStub{}, &diagnosticObjectsStub{}
	service, err := diagnosticuploads.New(repository, objects)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schema":"paperboat.diagnostic-upload-intent-request/v1","correlation_id":"support_01234567-89ab-4def-8123-456789abcdef","bytes":1024,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","categories":["manifest","recent_events","redacted_events","status"]}`)
	request := authenticatedDiagnosticRequest(http.MethodPost, "/v1/diagnostic-upload-intents", body)
	request.Header.Set("Idempotency-Key", "diagnostic-operation-0001")
	response := httptest.NewRecorder()
	diagnosticUploadIntentCreate(service).ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data diagnosticUploadIntentResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Schema != "paperboat.diagnostic-upload-intent/v1" || envelope.Data.State != "pending" || envelope.Data.UploadMethod != http.MethodPut || envelope.Data.UploadHeaders["If-None-Match"] != "*" {
		t.Fatalf("response=%#v", envelope.Data)
	}
	copy(objects.metadata.SHA256[:], repository.intent.SHA256[:])
	objects.metadata.Bytes, objects.metadata.ETag = repository.intent.Bytes, "etag"
	completeBody := []byte(`{}`)
	complete := authenticatedDiagnosticRequest(http.MethodPost, "/v1/diagnostic-upload-intents/"+repository.intent.ID+"/complete", completeBody)
	complete.SetPathValue("intent_id", repository.intent.ID)
	completed := httptest.NewRecorder()
	diagnosticUploadIntentComplete(service).ServeHTTP(completed, complete)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", completed.Code, completed.Body.String())
	}
	if !bytes.Contains(completed.Body.Bytes(), []byte(`"state":"uploaded"`)) || !bytes.Contains(completed.Body.Bytes(), []byte(repository.intent.CorrelationID)) {
		t.Fatalf("completion=%s", completed.Body.String())
	}
}

func TestDiagnosticUploadHandlerRejectsLooseInput(t *testing.T) {
	service, _ := diagnosticuploads.New(&diagnosticRepositoryStub{}, &diagnosticObjectsStub{})
	valid := []byte(`{"schema":"paperboat.diagnostic-upload-intent-request/v1","correlation_id":"support_01234567-89ab-4def-8123-456789abcdef","bytes":1024,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","categories":["manifest","recent_events","redacted_events","status"]}`)
	for name, mutate := range map[string]func(*http.Request){
		"content type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"query":        func(r *http.Request) { r.URL.RawQuery = "debug=true" },
		"malformed JSON": func(r *http.Request) {
			changed := []byte(`{"schema":`)
			r.Body = io.NopCloser(bytes.NewReader(changed))
			r.ContentLength = int64(len(changed))
		},
		"unknown field": func(r *http.Request) {
			changed := append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"token":"secret"}`)...)
			r.Body = io.NopCloser(bytes.NewReader(changed))
			r.ContentLength = int64(len(changed))
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := authenticatedDiagnosticRequest(http.MethodPost, "/v1/diagnostic-upload-intents", valid)
			request.Header.Set("Idempotency-Key", "diagnostic-operation-0001")
			mutate(request)
			response := httptest.NewRecorder()
			diagnosticUploadIntentCreate(service).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDiagnosticUploadPureOutcomesKeepStatusesAndMixedCausesFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "invalid", err: diagnosticuploads.ErrInvalid, status: http.StatusBadRequest, code: "validation_failed"},
		{name: "idempotency conflict", err: diagnosticuploads.ErrIdempotencyConflict, status: http.StatusConflict, code: "idempotency_key_conflict"},
		{name: "not found", err: diagnosticuploads.ErrNotFound, status: http.StatusNotFound, code: "diagnostic_upload_not_found"},
		{name: "expired", err: diagnosticuploads.ErrExpired, status: http.StatusGone, code: "diagnostic_upload_expired"},
		{name: "upload mismatch", err: diagnosticuploads.ErrUploadMismatch, status: http.StatusUnprocessableEntity, code: "diagnostic_upload_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, _, _ := diagnosticRequest("/v1/diagnostic-upload-intents")
			response := httptest.NewRecorder()
			writeDiagnosticUploadError(response, request, fmt.Errorf("safe wrapper: %w", tc.err))
			if response.Code != tc.status {
				t.Fatalf("pure outcome status=%d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tc.code {
				t.Fatalf("pure outcome code=%q, want %q", body.Error.Code, tc.code)
			}

			mixed := errors.Join(tc.err, syscall.EIO)
			response = httptest.NewRecorder()
			writeDiagnosticUploadError(response, request, mixed)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("mixed outcome status=%d, want 503: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDiagnosticUploadInvalidObjectAuthorityIsUnavailable(t *testing.T) {
	objects := &diagnosticObjectsStub{authority: diagnosticuploads.UploadAuthority{
		URL: "https://objects.example.test/upload", Headers: map[string]string{"X-Amz-Header": "bad\r\nvalue"},
	}}
	service, err := diagnosticuploads.New(&diagnosticRepositoryStub{}, objects)
	if err != nil {
		t.Fatal(err)
	}
	request := authenticatedDiagnosticRequest(http.MethodPost, "/v1/diagnostic-upload-intents", validDiagnosticUploadRequestBody())
	request.Header.Set("Idempotency-Key", "diagnostic-operation-0001")
	response := httptest.NewRecorder()
	diagnosticUploadIntentCreate(service).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid provider authority status=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "diagnostic_upload_unavailable" {
		t.Fatalf("invalid provider authority code=%q", body.Error.Code)
	}
}

func TestDiagnosticUploadObjectStoreFailureKeepsCauseAtHTTPOwner(t *testing.T) {
	const privateCanary = "S3_PRIVATE_ENDPOINT_SECRET"
	objects := &diagnosticObjectsStub{authorizeErr: errors.Join(syscall.EIO, errors.New(privateCanary))}
	service, err := diagnosticuploads.New(&diagnosticRepositoryStub{}, objects)
	if err != nil {
		t.Fatal(err)
	}
	request, logs, reference := diagnosticRequest("/v1/diagnostic-upload-intents")
	request.Method = http.MethodPost
	request.Body = io.NopCloser(bytes.NewReader(validDiagnosticUploadRequestBody()))
	request.ContentLength = int64(len(validDiagnosticUploadRequestBody()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "diagnostic-operation-0001")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{
		User: auth.User{ID: "usr_test"}, Client: &auth.ClientPrincipal{SessionID: "cli_test"},
	}))
	response := httptest.NewRecorder()
	diagnosticUploadIntentCreate(service).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("object-store status=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "diagnostic_upload_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("safe object-store response=%+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode single structured fault event: %v (%s)", err, logs.String())
	}
	if event["code"] != "diagnostic_upload_unavailable" || event["support_reference"] != reference || event["cause"] == "" || event["errno"] != float64(syscall.EIO) {
		t.Fatalf("object-store fault evidence=%v", event)
	}
	if strings.Contains(logs.String(), privateCanary) || strings.Contains(response.Body.String(), privateCanary) {
		t.Fatal("diagnostic upload object-store failure exposed private error text")
	}
}

func TestDiagnosticUploadMixedNotFoundReachesCorrelatedFaultOwner(t *testing.T) {
	const privateCanary = "DIAGNOSTIC_UPLOAD_PRIVATE_CAUSE"
	repository := &diagnosticRepositoryStub{getErr: errors.Join(diagnosticuploads.ErrNotFound, syscall.EIO, errors.New(privateCanary))}
	service, err := diagnosticuploads.New(repository, &diagnosticObjectsStub{})
	if err != nil {
		t.Fatal(err)
	}
	request, logs, reference := diagnosticRequest("/v1/diagnostic-upload-intents/diag_private_01/complete")
	request.Method = http.MethodPost
	request.Body = io.NopCloser(strings.NewReader("{}"))
	request.ContentLength = 2
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("intent_id", "diag_private_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{
		User: auth.User{ID: "usr_test"}, Client: &auth.ClientPrincipal{SessionID: "cli_test"},
	}))
	response := httptest.NewRecorder()
	diagnosticUploadIntentComplete(service).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("mixed not-found status=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "diagnostic_upload_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("safe retry response=%+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode single structured fault event: %v (%s)", err, logs.String())
	}
	for key, want := range map[string]string{
		"component": "paperboat-server", "stage": "http_handler", "code": "diagnostic_upload_unavailable",
		"support_reference": reference, "correlation_id": "correlation_fault_01", "outcome": "failed",
	} {
		if event[key] != want {
			t.Errorf("event[%q]=%v, want %q; event=%v", key, event[key], want, event)
		}
	}
	if event["error_type"] == "" || event["cause"] == "" || event["errno"] != float64(syscall.EIO) {
		t.Errorf("fault event lost bounded EIO evidence: %v", event)
	}
	if strings.Contains(logs.String(), privateCanary) || strings.Contains(response.Body.String(), privateCanary) {
		t.Fatal("diagnostic upload failure exposed private error text")
	}
}

func TestDiagnosticUploadBodyReadFailureIsNotMalformedJSON(t *testing.T) {
	const privateCanary = "DIAGNOSTIC_UPLOAD_BODY_PRIVATE"
	service, err := diagnosticuploads.New(&diagnosticRepositoryStub{}, &diagnosticObjectsStub{})
	if err != nil {
		t.Fatal(err)
	}
	request, logs, reference := diagnosticRequest("/v1/diagnostic-upload-intents")
	request.Method = http.MethodPost
	request.Body = diagnosticUploadReadError{err: errors.Join(syscall.EIO, errors.New(privateCanary))}
	request.ContentLength = 2
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "diagnostic-operation-0001")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{
		User: auth.User{ID: "usr_test"}, Client: &auth.ClientPrincipal{SessionID: "cli_test"},
	}))
	response := httptest.NewRecorder()
	diagnosticUploadIntentCreate(service).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("body read status=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "diagnostic_upload_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("safe body-read response=%+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode single structured fault event: %v (%s)", err, logs.String())
	}
	if event["code"] != "diagnostic_upload_unavailable" || event["support_reference"] != reference || event["cause"] == "" || event["errno"] != float64(syscall.EIO) {
		t.Fatalf("body read fault evidence=%v", event)
	}
	if strings.Contains(logs.String(), privateCanary) || strings.Contains(response.Body.String(), privateCanary) {
		t.Fatal("diagnostic upload body failure exposed private error text")
	}
}

type diagnosticUploadReadError struct{ err error }

func (r diagnosticUploadReadError) Read([]byte) (int, error) { return 0, r.err }
func (diagnosticUploadReadError) Close() error               { return nil }

func validDiagnosticUploadRequestBody() []byte {
	return []byte(`{"schema":"paperboat.diagnostic-upload-intent-request/v1","correlation_id":"support_01234567-89ab-4def-8123-456789abcdef","bytes":1024,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","categories":["manifest","recent_events","redacted_events","status"]}`)
}

func authenticatedDiagnosticRequest(method, target string, body []byte) *http.Request {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	client := &auth.ClientPrincipal{SessionID: "cli_test", Scopes: []string{"diagnostics:upload"}}
	ctx := context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_test"}, Client: client})
	return request.WithContext(ctx)
}
