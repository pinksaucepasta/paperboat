package privateaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type privateAccessReportStore struct{}

func (privateAccessReportStore) Save(context.Context, supportreport.Record) error { return nil }
func (privateAccessReportStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (privateAccessReportStore) Cleanup(context.Context) error { return nil }

type privateAccessAuditRecorder struct {
	records []AuditRecord
	err     error
}

func (a *privateAccessAuditRecorder) Record(_ context.Context, record AuditRecord) error {
	a.records = append(a.records, record)
	return a.err
}

func privateAccessDiagnosticContext(t *testing.T) (context.Context, *supportreport.Service, *bytes.Buffer, string) {
	t.Helper()
	reports, err := supportreport.New(privateAccessReportStore{}, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reports.Close)
	var logs bytes.Buffer
	reports.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	reference := supportreport.NewReference()
	ctx := reports.Context(context.Background(), reference)
	ctx = supportreport.WithFailureIdentity(ctx, "acct_1", "request_001", reference)
	ctx = serverdiagnostics.WithCorrelationID(ctx, "correlation_private_access_01")
	return ctx, reports, &logs, reference
}

func testPrivateTunnelTCP(now time.Time) (Request, Binding) {
	request := testRequest(now)
	request.ResourceKind = ResourceTunnel
	request.ResourceID = "tunnel_1"
	request.OperationID = ""
	request.ConnectorID = "connector_1"
	request.Audience = AudienceTunnelTCP
	request.Protocol = ProtocolTCP
	request.Method, request.Host, request.Path = "", "", ""
	binding := testBinding(now)
	binding.ResourceKind = ResourceTunnel
	binding.ResourceID = request.ResourceID
	binding.OperationID = ""
	binding.ConnectorID = request.ConnectorID
	binding.Protocol = ProtocolTCP
	binding.Hostname, binding.PathPrefix = "", ""
	return request, binding
}

func TestDeniedErrorRetainsCauseWithoutChangingItsPublicText(t *testing.T) {
	private := errors.New("PRIVATE_AUTHORITY_DETAIL")
	cause := errors.Join(syscall.ECONNREFUSED, private)
	denied := newDeniedWithCause(ReasonRoutePaused, cause)
	if denied.DiagnosticCause() != cause || !errors.Is(denied, ErrDenied) || !errors.Is(denied, syscall.ECONNREFUSED) {
		t.Fatalf("denial did not preserve its sentinels and original cause: %#v", denied)
	}
	if got, want := denied.Error(), "private access denied: route_paused"; got != want || strings.Contains(got, "PRIVATE_AUTHORITY_DETAIL") {
		t.Fatalf("public denial error changed or exposed the cause: %q", got)
	}
}

func privateAccessHTTPRequest(t *testing.T, request Request, ctx context.Context) *http.Request {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, AuthorizePath, bytes.NewReader(body)).WithContext(ctx)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Paperboat-Private-Access-Grant", "signed_grant")
	return httpRequest
}

func TestPrivateAccessAuthorityOutagePreservesCauseAndRecovers(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	resolver := &fakeResolver{binding: binding, err: errors.Join(ErrRouteUnavailable, syscall.ECONNREFUSED, errors.New("PRIVATE_AUTHORITY_DETAIL"))}
	grants := &fakeGrant{request: request, token: "signed_grant"}
	audit := &privateAccessAuditRecorder{}
	service, err := NewService(resolver, audit, Config{Now: testClock, GrantVerifier: grants, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, privateAccessHTTPRequest(t, request, ctx))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("authority outage status/body=%d/%s", response.Code, response.Body.String())
	}
	var denied Decision
	if err := json.Unmarshal(response.Body.Bytes(), &denied); err != nil {
		t.Fatal(err)
	}
	if denied.Allowed || denied.Reason != ReasonInternal {
		t.Fatalf("public denial projection changed during outage: %#v", denied)
	}
	for _, value := range []string{"connection_refused", `"http_status":503`, reference, "correlation_private_access_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("safe support log omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(response.Body.String()+logs.String(), "PRIVATE_AUTHORITY_DETAIL") {
		t.Fatal("authority failure exposed arbitrary error text")
	}
	if len(audit.records) != 1 || audit.records[0].Allowed || audit.records[0].Reason != ReasonInternal {
		t.Fatalf("failed authority lookup was not recorded once: %#v", audit.records)
	}

	resolver.err = nil
	recovered := httptest.NewRecorder()
	handler.ServeHTTP(recovered, privateAccessHTTPRequest(t, request, ctx))
	if recovered.Code != http.StatusOK {
		t.Fatalf("authority recovery status/body=%d/%s", recovered.Code, recovered.Body.String())
	}
	var allowed Decision
	if err := json.Unmarshal(recovered.Body.Bytes(), &allowed); err != nil {
		t.Fatal(err)
	}
	if !allowed.Allowed || resolver.calls != 2 {
		t.Fatalf("authority recovery failed or reused a stale denial: allowed=%v resolver_calls=%d", allowed.Allowed, resolver.calls)
	}
}

func TestPrivateAccessMixedMissingAndOperationalCauseRemainsUnavailable(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	private := errors.New("PRIVATE_MISSING_AUTHORITY_DETAIL")
	resolver := &fakeResolver{binding: binding, err: errors.Join(ErrResourceNotFound, syscall.EIO, private)}
	grants := &fakeGrant{request: request, token: "signed_grant"}
	service, err := NewService(resolver, nil, Config{Now: testClock, GrantVerifier: grants, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, privateAccessHTTPRequest(t, request, ctx))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("mixed missing/outage status/body=%d/%s", response.Code, response.Body.String())
	}
	var denied Decision
	if err := json.Unmarshal(response.Body.Bytes(), &denied); err != nil {
		t.Fatal(err)
	}
	if denied.Allowed || denied.Reason != ReasonInternal {
		t.Fatalf("mixed error was projected as a benign resource denial: %#v", denied)
	}
	for _, value := range []string{"syscall.Errno", `"http_status":503`, reference} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("mixed missing/outage diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(response.Body.String()+logs.String(), "PRIVATE_MISSING_AUTHORITY_DETAIL") {
		t.Fatal("mixed missing/outage exposed private error prose")
	}

	resolver.err = nil
	recovered := httptest.NewRecorder()
	handler.ServeHTTP(recovered, privateAccessHTTPRequest(t, request, ctx))
	if recovered.Code != http.StatusOK || resolver.calls != 2 {
		t.Fatalf("healthy retry did not recover: status=%d resolver_calls=%d body=%s", recovered.Code, resolver.calls, recovered.Body.String())
	}
}

func TestPrivateAccessMixedMachineDenialAndIORemainsOperational(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	private := errors.New("PRIVATE_MACHINE_STATE_DETAIL")
	state := &fakeMachineState{err: errors.Join(newDenied(ReasonMachineRevoked), syscall.EIO, private)}
	grants := &fakeGrant{request: request, token: "signed_grant"}
	service, err := NewService(&fakeResolver{binding: binding}, nil, Config{Now: testClock, GrantVerifier: grants, MachineState: state})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, privateAccessHTTPRequest(t, request, ctx))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("mixed machine denial/outage status/body=%d/%s", response.Code, response.Body.String())
	}
	for _, value := range []string{"syscall.Errno", `"http_status":503`, reference} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("mixed machine denial diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(response.Body.String()+logs.String(), "PRIVATE_MACHINE_STATE_DETAIL") {
		t.Fatal("mixed machine denial exposed private error prose")
	}

	state.err = nil
	recovered := httptest.NewRecorder()
	handler.ServeHTTP(recovered, privateAccessHTTPRequest(t, request, ctx))
	if recovered.Code != http.StatusOK {
		t.Fatalf("machine-state recovery status/body=%d/%s", recovered.Code, recovered.Body.String())
	}
}

type privateAccessReadFailure struct {
	data []byte
	err  error
	done bool
}

func (r *privateAccessReadFailure) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), nil
}

type privateAccessReadCloser struct{ io.Reader }

func (privateAccessReadCloser) Close() error { return nil }

func TestPrivateAccessRequestReadFailureIsUnavailableAndActionable(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	grants := &fakeGrant{request: request, token: "signed_grant"}
	service, err := NewService(&fakeResolver{binding: binding}, nil, Config{Now: testClock, GrantVerifier: grants, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, AuthorizePath, nil).WithContext(ctx)
	httpRequest.Body = privateAccessReadCloser{Reader: &privateAccessReadFailure{data: body, err: syscall.EIO}}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Paperboat-Private-Access-Grant", "signed_grant")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("body I/O failure status/body=%d/%s", response.Code, response.Body.String())
	}
	for _, value := range []string{"syscall.Errno", `"http_status":503`, reference} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("body I/O failure diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(response.Body.String()+logs.String(), "PRIVATE") {
		t.Fatal("body I/O failure exposed private error prose")
	}
}

type privateAccessFailingResponseWriter struct {
	header http.Header
	status int
	err    error
}

func (w *privateAccessFailingResponseWriter) Header() http.Header    { return w.header }
func (w *privateAccessFailingResponseWriter) WriteHeader(status int) { w.status = status }
func (w *privateAccessFailingResponseWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestPrivateAccessSuccessWriteFailureIsReported(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	grants := &fakeGrant{request: request, token: "signed_grant"}
	service, err := NewService(&fakeResolver{binding: binding}, nil, Config{Now: testClock, GrantVerifier: grants, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	private := errors.New("PRIVATE_RESPONSE_WRITER_DETAIL")
	writer := &privateAccessFailingResponseWriter{header: make(http.Header), err: errors.Join(syscall.EIO, private)}
	handler.ServeHTTP(writer, privateAccessHTTPRequest(t, request, ctx))
	if writer.status != http.StatusOK {
		t.Fatalf("successful authorization status=%d, want 200", writer.status)
	}
	for _, value := range []string{"syscall.Errno", reference} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("response write diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(logs.String(), "PRIVATE_RESPONSE_WRITER_DETAIL") {
		t.Fatal("response writer exposed private error prose")
	}
}

func TestPrivateAccessErrorResponseWriteFailureIsReported(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	grants := &fakeGrant{request: request, token: "signed_grant"}
	service, err := NewService(&fakeResolver{binding: binding}, nil, Config{Now: testClock, GrantVerifier: grants, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	requestWithoutGrant := func() *http.Request {
		httpRequest := privateAccessHTTPRequest(t, request, ctx)
		httpRequest.Header.Del("X-Paperboat-Private-Access-Grant")
		return httpRequest
	}

	ordinary := httptest.NewRecorder()
	handler.ServeHTTP(ordinary, requestWithoutGrant())
	if ordinary.Code != http.StatusUnauthorized || logs.Len() != 0 {
		t.Fatalf("pure grant denial response was reported before I/O failed: status=%d logs=%s", ordinary.Code, logs.String())
	}

	private := errors.New("PRIVATE_ERROR_RESPONSE_WRITER_DETAIL")
	writer := &privateAccessFailingResponseWriter{header: make(http.Header), err: errors.Join(syscall.EIO, private)}
	handler.ServeHTTP(writer, requestWithoutGrant())
	if writer.status != http.StatusUnauthorized {
		t.Fatalf("grant denial status=%d, want 401", writer.status)
	}
	for _, value := range []string{"syscall.Errno", `"http_status":401`, reference} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("error response write diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Count(logs.String(), `"outcome":"failed"`) != 1 || strings.Contains(logs.String(), "PRIVATE_ERROR_RESPONSE_WRITER_DETAIL") {
		t.Fatalf("error response write was not reported once with bounded prose: %s", logs.String())
	}
}

func TestGrantHTTPMixedIdempotencyConflictRemainsOperationalAndRecovers(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	issue := GrantIssueRequest{
		ResourceKind: request.ResourceKind, ResourceID: request.ResourceID, RouteID: request.RouteID,
		Audience: request.Audience, ExpiresAt: request.ExpiresAt, Nonce: request.Nonce,
		ConnectorID: request.ConnectorID, CarrierSessionID: request.CarrierSessionID,
		RouteGeneration: request.RouteGeneration, SessionGeneration: request.SessionGeneration,
		ProcessGeneration: request.ProcessGeneration, ConfigGeneration: request.ConfigGeneration,
		AssignmentGeneration: request.AssignmentGeneration, EdgeNodeID: request.EdgeNodeID,
		EdgeProcessEpoch: request.EdgeProcessEpoch, Protocol: request.Protocol,
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, CorrelationID: request.CorrelationID,
	}
	body, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	private := errors.New("PRIVATE_GRANT_IDEMPOTENCY_DETAIL")
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "pure conflict", err: ErrIdempotencyConflict, want: http.StatusConflict},
		{name: "conflict with IO failure", err: errors.Join(ErrIdempotencyConflict, syscall.EIO, private), want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minter := &fakeGrant{token: "signed_grant", mintErr: tc.err}
			service, err := NewService(&fakeResolver{binding: binding}, nil, Config{Now: testClock, GrantMinter: minter, MachineState: &fakeMachineState{}})
			if err != nil {
				t.Fatal(err)
			}
			claims := controlplane.MachineRequestClaims{
				UserID: request.AccountID, MachineID: request.MachineID,
				InstallationGeneration: int64(request.InstallationGeneration), SessionGeneration: int64(request.SessionGeneration),
				CredentialJTI: request.SessionID, OperationID: request.IdempotencyKey,
			}
			handler, err := NewGrantHTTPHandler(service, &fakeMachineRequest{claims: claims})
			if err != nil {
				t.Fatal(err)
			}
			ctx, _, logs, reference := privateAccessDiagnosticContext(t)
			req := func() *http.Request {
				httpRequest := httptest.NewRequest(http.MethodPost, GrantPath, bytes.NewReader(body)).WithContext(ctx)
				httpRequest.Header.Set("Content-Type", "application/json")
				httpRequest.Header.Set("Authorization", "Bearer machine_control_token")
				httpRequest.Header.Set("X-Paperboat-Machine-Identity", "machine_control_token")
				httpRequest.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
				return httpRequest
			}

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req())
			if response.Code != tc.want {
				t.Fatalf("mixed idempotency status/body=%d/%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String()+logs.String(), "PRIVATE_GRANT_IDEMPOTENCY_DETAIL") {
				t.Fatal("idempotency failure exposed private error prose")
			}
			if tc.want == http.StatusServiceUnavailable {
				for _, value := range []string{"syscall.Errno", `"http_status":503`, reference} {
					if !strings.Contains(logs.String(), value) {
						t.Errorf("mixed conflict diagnostics omitted %q: %s", value, logs.String())
					}
				}
				if strings.Count(logs.String(), `"outcome":"failed"`) != 1 {
					t.Fatalf("mixed conflict was not reported once: %s", logs.String())
				}
			}

			minter.mintErr = nil
			recovered := httptest.NewRecorder()
			handler.ServeHTTP(recovered, req())
			if recovered.Code != http.StatusOK {
				t.Fatalf("grant issuance did not recover: status/body=%d/%s", recovered.Code, recovered.Body.String())
			}
		})
	}
}

func TestPrivateAccessVerifierDistinguishesRejectionFromOutage(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	service, err := NewService(&fakeResolver{binding: binding}, nil, Config{Now: testClock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	secret := errors.New("PRIVATE_EDGE_DATABASE_DETAIL")
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "invalid credential", err: previewattachment.ErrUnauthorized, want: http.StatusUnauthorized},
		{name: "database outage", err: errors.Join(syscall.ECONNREFUSED, secret), want: http.StatusServiceUnavailable},
		{name: "credential rejection joined with I/O failure", err: errors.Join(previewattachment.ErrUnauthorized, syscall.EIO), want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewHTTPHandler(service, fakeEdge{err: tc.err})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, AuthorizePath, bytes.NewReader(raw)).WithContext(ctx)
			httpRequest.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httpRequest)
			if response.Code != tc.want {
				t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
			}
		})
	}
	if !strings.Contains(logs.String(), "connection_refused") || !strings.Contains(logs.String(), reference) || !strings.Contains(logs.String(), "correlation_private_access_01") {
		t.Fatalf("edge database outage log lost actionable cause or request correlation: %s", logs.String())
	}
	if strings.Contains(logs.String()+"\n", "PRIVATE_EDGE_DATABASE_DETAIL") {
		t.Fatal("edge verifier failure exposed arbitrary error text")
	}
	if strings.Count(logs.String(), `"outcome":"failed"`) != 2 || strings.Count(logs.String(), `"outcome":"rejected"`) != 1 {
		t.Fatalf("verifier outcomes were not logged once as failure plus expected rejection: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "syscall.Errno") || !strings.Contains(logs.String(), `"http_status":503`) {
		t.Fatalf("joined I/O failure lost its bounded cause or 503 status: %s", logs.String())
	}
}

func TestPrivateAccessAuditFailureKeepsOriginalCause(t *testing.T) {
	now := testClock()
	request, binding := testPrivateTunnelTCP(now)
	private := errors.New("PRIVATE_AUDIT_DATABASE_DETAIL")
	audit := &privateAccessAuditRecorder{err: errors.Join(syscall.ECONNREFUSED, private)}
	grants := &fakeGrant{request: request, token: "signed_grant"}
	service, err := NewService(&fakeResolver{binding: binding}, audit, Config{Now: testClock, GrantVerifier: grants, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, privateAccessHTTPRequest(t, request, ctx))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit outage status/body=%d/%s", response.Code, response.Body.String())
	}
	for _, value := range []string{"connection_refused", reference, "correlation_private_access_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("audit failure log omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(response.Body.String()+logs.String(), "PRIVATE_AUDIT_DATABASE_DETAIL") {
		t.Fatal("audit failure exposed arbitrary error text")
	}
}

func TestGrantIssueVerifierOutageIsActionableAndInvalidProofStaysRejected(t *testing.T) {
	now := testClock()
	request, _ := testPrivateTunnelTCP(now)
	issue := GrantIssueRequest{
		ResourceKind: request.ResourceKind, ResourceID: request.ResourceID, RouteID: request.RouteID,
		Audience: request.Audience, ExpiresAt: request.ExpiresAt, Nonce: request.Nonce,
		ConnectorID: request.ConnectorID, CarrierSessionID: request.CarrierSessionID,
		RouteGeneration: request.RouteGeneration, SessionGeneration: request.SessionGeneration,
		ProcessGeneration: request.ProcessGeneration, ConfigGeneration: request.ConfigGeneration,
		AssignmentGeneration: request.AssignmentGeneration, EdgeNodeID: request.EdgeNodeID,
		EdgeProcessEpoch: request.EdgeProcessEpoch, Protocol: request.Protocol,
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, CorrelationID: request.CorrelationID,
	}
	body, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(&fakeResolver{}, nil, Config{Now: testClock})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	secret := errors.New("PRIVATE_MACHINE_AUTHORITY_DETAIL")
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "machine database unavailable", err: errors.Join(syscall.ECONNREFUSED, secret), want: http.StatusServiceUnavailable},
		{name: "invalid proof", err: controlplane.ErrHelperProof, want: http.StatusUnauthorized},
		{name: "invalid proof joined with I/O failure", err: errors.Join(controlplane.ErrHelperProof, syscall.EIO), want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := &fakeMachineRequest{err: tc.err}
			handler, err := NewGrantHTTPHandler(service, machine)
			if err != nil {
				t.Fatal(err)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, GrantPath, bytes.NewReader(body)).WithContext(ctx)
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("Authorization", "Bearer machine_control_token")
			httpRequest.Header.Set("X-Paperboat-Machine-Identity", "machine_control_token")
			httpRequest.Header.Set("X-Paperboat-Machine-Proof", "cHJvb2Y")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httpRequest)
			if response.Code != tc.want {
				t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
			}
		})
	}
	for _, value := range []string{"connection_refused", reference, "correlation_private_access_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("machine verifier diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(logs.String(), "PRIVATE_MACHINE_AUTHORITY_DETAIL") {
		t.Fatal("machine verifier failure exposed arbitrary error text")
	}
	if strings.Count(logs.String(), `"outcome":"failed"`) != 2 || strings.Count(logs.String(), `"outcome":"rejected"`) != 1 {
		t.Fatalf("machine verifier outcomes were not recorded once: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "syscall.Errno") || !strings.Contains(logs.String(), `"http_status":503`) {
		t.Fatalf("joined proof and I/O failure lost its bounded cause or 503 status: %s", logs.String())
	}
}

func TestSQLPrivateAccessAuthorityErrorsRemainOperational(t *testing.T) {
	database, err := db.Open(config.Database{Driver: "postgres", DSN: "postgres://paperboat:paperboat@127.0.0.1:1/privateaccess_test?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	verifier, err := NewSQLMachineStateVerifier(database)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{AccountID: "acct_1", MachineID: "machine_1", SessionID: "session_1", InstallationGeneration: 1}
	err = verifier.VerifyCurrentMachine(context.Background(), identity, testClock())
	if !errors.Is(err, ErrIdentityUnavailable) || err == ErrIdentityUnavailable {
		t.Fatalf("closed authority database was flattened or misclassified: %v", err)
	}
	var verifierCause interface{ Unwrap() []error }
	if !errors.As(err, &verifierCause) || len(verifierCause.Unwrap()) != 2 || verifierCause.Unwrap()[1] == nil {
		t.Fatalf("closed authority database cause was not retained: %v", err)
	}

	request, _ := testPrivateTunnelTCP(testClock())
	resolver := &SQLResolver{db: database}
	_, err = resolver.resolveTunnel(context.Background(), Lookup{AccountID: request.AccountID, Request: request, Edge: EdgeIdentity{NodeID: request.EdgeNodeID, ProcessEpoch: request.EdgeProcessEpoch}}, testClock())
	if !errors.Is(err, ErrRouteUnavailable) || err == ErrRouteUnavailable {
		t.Fatalf("closed route authority database was flattened or misclassified: %v", err)
	}
	var resolverCause interface{ Unwrap() []error }
	if !errors.As(err, &resolverCause) || len(resolverCause.Unwrap()) != 2 || resolverCause.Unwrap()[1] == nil {
		t.Fatalf("closed route authority database cause was not retained: %v", err)
	}
}
