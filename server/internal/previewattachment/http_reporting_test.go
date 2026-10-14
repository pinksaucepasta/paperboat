package previewattachment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type attachmentFaultStore struct{ records chan supportreport.Record }

func (s attachmentFaultStore) Save(ctx context.Context, record supportreport.Record) error {
	select {
	case s.records <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (attachmentFaultStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (attachmentFaultStore) Cleanup(context.Context) error { return nil }

type attachmentVerifierFailure struct{ err error }

func (v attachmentVerifierFailure) VerifyMachineRequest(context.Context, *http.Request, []byte) (MachineProof, error) {
	return MachineProof{}, v.err
}

type attachmentPreconditionOK struct{}

func (attachmentPreconditionOK) CheckPreviewLeaseIfMatch(context.Context, MachineProof, string, string) error {
	return nil
}

func TestAttachmentHTTPFailureKeepsCauseWhenSentryIsDisabled(t *testing.T) {
	store := attachmentFaultStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	ref := supportreport.NewReference()
	ctx, _ := reporter.Begin(context.Background(), "http_previews", ref, "")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "request_attachment_1")
	req := httptest.NewRequest("POST", "/v1/previews/preview_1/attachment", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	writeAttachmentServiceError(response, req, fmt.Errorf("storage unavailable: %w", errors.Join(ErrAdmissionUnavailable, syscall.ETIMEDOUT)))
	if response.Code != 503 {
		t.Fatalf("attachment failure status changed: status=%d body=%s", response.Code, response.Body.String())
	}
	var record supportreport.Record
	select {
	case record = <-store.records:
	case <-time.After(time.Second):
		t.Fatal("attachment failure was not persisted")
	}
	if record.Cause != "network_timeout" || record.Code != "edge_admission_unavailable" || record.Reference != ref || record.CorrelationID != "request_attachment_1" || record.Severity != "error" || record.Reporting != "disabled" {
		t.Fatalf("bounded attachment diagnostic lost cause or correlation: %+v", record)
	}
	if strings.Contains(response.Body.String(), "storage unavailable") || strings.Contains(response.Body.String(), "ETIMEDOUT") {
		t.Fatal("attachment response disclosed internal cause")
	}
}

func TestAttachmentVerifierDatabaseFailureIsUnavailableNotRejected(t *testing.T) {
	store := attachmentFaultStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	service, err := NewService(&serviceRepositoryFake{}, &testAuthority{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	serve := func(cause error) *httptest.ResponseRecorder {
		t.Helper()
		reference := supportreport.NewReference()
		ctx, _ := reporter.Begin(context.Background(), "http_previews", reference, "")
		ctx = serverdiagnostics.WithCorrelationID(ctx, "request_attachment_proof_1")
		request := httptest.NewRequest("POST", "/v1/previews/preview_1/carrier-attachment", strings.NewReader("{}"))
		request = request.WithContext(ctx)
		request.Header.Set("Authorization", "Bearer machine-identity")
		request.Header.Set("X-Paperboat-Machine-Identity", "machine-identity")
		request.Header.Set("X-Paperboat-Machine-Proof", "cHJvb2Y")
		handler, err := NewHTTPHandler(service, attachmentVerifierFailure{err: cause}, attachmentPreconditionOK{})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	secret := "SECRET_PROOF_DATABASE_DSN"
	response := serve(fmt.Errorf("dsn %s: %w", secret, syscall.ECONNRESET))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("database failure response=%d body=%s", response.Code, response.Body.String())
	}
	var record supportreport.Record
	select {
	case record = <-store.records:
	case <-time.After(time.Second):
		t.Fatal("machine proof database failure was not recorded")
	}
	if record.Code != "authorization_unavailable" || record.Cause != "connection_reset" || record.HTTPStatus != 503 || record.CorrelationID != "request_attachment_proof_1" {
		t.Fatalf("machine proof failure diagnostic lost typed cause/correlation: %+v", record)
	}
	if got := serve(ErrUnauthorized); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected proof rejection status=%d body=%s", got.Code, got.Body.String())
	}
	if mixed := serve(errors.Join(ErrUnauthorized, syscall.EIO)); mixed.Code != http.StatusServiceUnavailable || strings.Contains(mixed.Body.String(), "input/output error") {
		t.Fatalf("mixed proof rejection and database failure status=%d body=%s", mixed.Code, mixed.Body.String())
	}
	select {
	case record = <-store.records:
		if record.Code != "authorization_unavailable" || record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("mixed proof verifier failure lost operational cause: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("mixed proof verifier failure was not recorded")
	}
}

func TestPreviewEdgeResolverFailurePreservesMixedCauseAtFinalHTTPOwner(t *testing.T) {
	store := attachmentFaultStore{records: make(chan supportreport.Record, 2)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	service, err := NewService(&serviceRepositoryFake{}, &testAuthority{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewEdgeHTTPHandler(service, &edgeOutboxFake{}, edgeVerifierFake{err: errors.Join(ErrUnauthorized, syscall.ECONNRESET)})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportreport.NewReference()
	ctx, _ := reporter.Begin(context.Background(), "http_edge", reference, "")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "edge_auth_request_1")
	request := httptest.NewRequest(http.MethodPost, EdgeAdmissionPullPath, strings.NewReader(`{"edge_node_id":"edge_01","edge_process_epoch":"edge-process-1"}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "connection reset") {
		t.Fatalf("mixed edge authentication failure = %d %s", response.Code, response.Body.String())
	}
	select {
	case record := <-store.records:
		if record.Reference != reference || record.CorrelationID != "edge_auth_request_1" || record.Code != "authorization_unavailable" || record.Cause != "connection_reset" || record.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("edge authentication diagnostic lost operational cause: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("edge authorization outage was not captured locally")
	}

	denialRequest := httptest.NewRequest(http.MethodPost, EdgeAdmissionPullPath, strings.NewReader(`{"edge_node_id":"edge_01","edge_process_epoch":"edge-process-1"}`)).WithContext(ctx)
	denial, err := NewEdgeHTTPHandler(service, &edgeOutboxFake{}, edgeVerifierFake{err: fmt.Errorf("wrapped: %w", ErrUnauthorized)})
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	denial.ServeHTTP(denied, denialRequest)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("pure identity rejection status = %d, want %d", denied.Code, http.StatusUnauthorized)
	}
	select {
	case record := <-store.records:
		t.Fatalf("pure identity rejection generated exception report: %+v", record)
	default:
	}
}

func (attachmentPreconditionOK) ProjectPreviewAttachmentEndpoint(_ context.Context, _ MachineProof, attachment Attachment) (Attachment, error) {
	return attachment, nil
}
