package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type edgeFailureReportStore struct{ saved chan supportreport.Record }

func (s *edgeFailureReportStore) Save(ctx context.Context, record supportreport.Record) error {
	select {
	case s.saved <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*edgeFailureReportStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}

func (*edgeFailureReportStore) Cleanup(context.Context) error { return nil }

func TestWriteEdgeErrorCapturesBoundedCauseWhenSentryIsDisabled(t *testing.T) {
	store := &edgeFailureReportStore{saved: make(chan supportreport.Record, 1)}
	reports, err := supportreport.New(store, supportreport.Config{Region: "test", Instance: "slot-01"})
	if err != nil {
		t.Fatal(err)
	}
	reports.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reports.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("reporter shutdown: %v", err)
		}
	}()

	reference := supportreport.NewReference()
	ctx := serverdiagnostics.WithRequestID(runCtx, "req_edge_fault")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_edge_fault")
	request := httptest.NewRequest(http.MethodGet, "/v1/trust/revocations", nil).WithContext(reports.Context(ctx, reference))
	response := httptest.NewRecorder()
	cause := fmt.Errorf("database endpoint SECRET_EDGE_DB: %w", syscall.ECONNRESET)
	writeEdgeError(response, request, http.StatusServiceUnavailable, "control_unavailable", true, 1000, cause)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "SECRET_EDGE_DB") {
		t.Fatalf("edge error response leaked its cause or used wrong status: %d %s", response.Code, response.Body.String())
	}
	select {
	case record := <-store.saved:
		encoded, _ := json.Marshal(record)
		if record.Reference != reference || record.RequestID != "req_edge_fault" || record.CorrelationID != "corr_edge_fault" || record.Code != "control_unavailable" || record.Cause != "connection_reset" || record.HTTPStatus != http.StatusServiceUnavailable || strings.Contains(string(encoded), "SECRET_EDGE_DB") {
			t.Fatalf("captured edge failure is incomplete or unsafe: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("edge failure was not persisted to local support reporting")
	}
	rejected := httptest.NewRecorder()
	writeEdgeError(rejected, request, http.StatusUnauthorized, "credential_invalid", false, 0, ErrHelperProof)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("expected credential rejection status %d, got %d", http.StatusUnauthorized, rejected.Code)
	}
	select {
	case record := <-store.saved:
		t.Fatalf("expected credential rejection generated an exception report: %+v", record)
	default:
	}
}

func TestEdgeHandlerCapturesResolverOutageWithoutChangingCredentialDenial(t *testing.T) {
	store := &edgeFailureReportStore{saved: make(chan supportreport.Record, 1)}
	reports, err := supportreport.New(store, supportreport.Config{Region: "test", Instance: "slot-01"})
	if err != nil {
		t.Fatal(err)
	}
	reports.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reports.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("reporter shutdown: %v", err)
		}
	}()

	service := NewEdgeService(nil, "managed-control-credential-012345678901")
	service.SetNodeCredentials(nodeauth.Composite{
		nodeCredentialError{err: errors.Join(nodeauth.ErrNotFound, syscall.ECONNREFUSED)},
		scopedNodeCredential{},
	})
	reference := supportreport.NewReference()
	ctx := serverdiagnostics.WithRequestID(runCtx, "req_node_auth_outage")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_node_auth_outage")
	request := httptest.NewRequest(http.MethodPost, "/v1/nodes/heartbeat", strings.NewReader(`{"edge_node_id":"node_a"}`)).WithContext(reports.Context(ctx, reference))
	request.Header.Set("Authorization", "Bearer node-secret")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "connection refused") {
		t.Fatalf("resolver outage response = %d %s", response.Code, response.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope["code"] != "control_unavailable" || envelope["requestId"] != "req_node_auth_outage" {
		t.Fatalf("resolver outage envelope = %#v, err=%v", envelope, err)
	}
	select {
	case record := <-store.saved:
		encoded, _ := json.Marshal(record)
		if record.Reference != reference || record.RequestID != "req_node_auth_outage" || record.CorrelationID != "corr_node_auth_outage" || record.Code != "control_unavailable" || record.Cause != "connection_refused" || record.Errno != int(syscall.ECONNREFUSED) || record.HTTPStatus != http.StatusServiceUnavailable || strings.Contains(string(encoded), "connection refused") {
			t.Fatalf("captured resolver outage is incomplete or unsafe: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver outage was not persisted to local support reporting")
	}

	service.SetNodeCredentials(scopedNodeCredential{})
	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/nodes/heartbeat", strings.NewReader(`{"edge_node_id":"node_a"}`)).WithContext(reports.Context(ctx, supportreport.NewReference()))
	unauthorized.Header.Set("Authorization", "Bearer unknown")
	denied := httptest.NewRecorder()
	service.Handler().ServeHTTP(denied, unauthorized)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("unknown credential status = %d, want %d", denied.Code, http.StatusUnauthorized)
	}
	select {
	case record := <-store.saved:
		t.Fatalf("expected credential rejection generated exception report: %+v", record)
	default:
	}
}

func TestEdgeHandlerCapturesScopedRequestBodyIOFailure(t *testing.T) {
	store := &edgeFailureReportStore{saved: make(chan supportreport.Record, 1)}
	reports, err := supportreport.New(store, supportreport.Config{Region: "test", Instance: "slot-01"})
	if err != nil {
		t.Fatal(err)
	}
	reports.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reports.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("reporter shutdown: %v", err)
		}
	}()

	service := NewEdgeService(nil, "managed-control-credential-012345678901")
	service.SetNodeCredentials(scopedNodeCredential{})
	reference := supportreport.NewReference()
	ctx := serverdiagnostics.WithRequestID(runCtx, "req_node_body_io")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_node_body_io")
	request := httptest.NewRequest(http.MethodPost, "/v1/nodes/heartbeat", nil).WithContext(reports.Context(ctx, reference))
	request.Header.Set("Authorization", "Bearer node-secret")
	request.Body = &requestBodyFailure{
		value:    []byte(`{"edge_node_id":"node_a"`),
		readErr:  syscall.EIO,
		closeErr: syscall.ECONNRESET,
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "connection reset") {
		t.Fatalf("body read failure response = %d %s", response.Code, response.Body.String())
	}
	select {
	case record := <-store.saved:
		if record.Reference != reference || record.RequestID != "req_node_body_io" || record.CorrelationID != "corr_node_body_io" || record.Code != "control_unavailable" || record.HTTPStatus != http.StatusServiceUnavailable || len(record.ErrorChain) == 0 || strings.Contains(record.Cause, "SECRET") {
			t.Fatalf("request body failure was not safely captured: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scoped request body failure was not persisted locally")
	}
}

func TestUsageErrorResponseClassifiesPermanentAndRetryableFailures(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{name: "invalid report", err: fmt.Errorf("wrapped: %w", ErrInvalidUsageReport), status: http.StatusBadRequest, code: "invalid_request"},
		{name: "invalid signature", err: ErrUsageSignature, status: http.StatusBadRequest, code: "credential_invalid"},
		{name: "operation conflict", err: ErrUsageOperationConflict, status: http.StatusConflict, code: "operation_conflict"},
		{name: "storage failure", err: errors.New("database unavailable"), status: http.StatusServiceUnavailable, code: "control_unavailable", retryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, retryable, retryAfterMS := usageErrorResponse(tt.err)
			if status != tt.status || code != tt.code || retryable != tt.retryable {
				t.Fatalf("response = (%d, %q, %t), want (%d, %q, %t)", status, code, retryable, tt.status, tt.code, tt.retryable)
			}
			if (retryAfterMS > 0) != retryable {
				t.Fatalf("retry_after_ms = %d, retryable = %t", retryAfterMS, retryable)
			}
		})
	}
}

func TestEdgeHandlerRejectsUnauthorizedAndUnknownRoutes(t *testing.T) {
	service := NewEdgeService(nil, "edge-control-credential-01234567890123456789")
	request := httptest.NewRequest(http.MethodPost, "/v1/nodes/register", strings.NewReader(`{}`)).WithContext(serverdiagnostics.WithRequestID(context.Background(), "req_edge_test"))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope["code"] != "unauthenticated" || envelope["requestId"] != "req_edge_test" || envelope["retryable"] != false {
		t.Fatalf("error envelope = %#v, %v", envelope, err)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/unknown", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer edge-control-credential-01234567890123456789")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", response.Code)
	}
}

func TestValidateCarrierEndpointRequiresSeparateCanonicalListener(t *testing.T) {
	valid := struct {
		Host     string `json:"host"`
		TCPPort  uint16 `json:"tcp_port"`
		QUICPort uint16 `json:"quic_port"`
	}{Host: "carrier.edge.example.test", TCPPort: 17443, QUICPort: 17444}
	if err := validateCarrierEndpoint(valid); err != nil {
		t.Fatalf("valid carrier endpoint error = %v", err)
	}
	if err := validateCarrierEndpoint(struct {
		Host     string `json:"host"`
		TCPPort  uint16 `json:"tcp_port"`
		QUICPort uint16 `json:"quic_port"`
	}{}); err == nil {
		t.Fatal("omitted canonical carrier endpoint was accepted")
	}
	for _, invalid := range []struct {
		Host     string `json:"host"`
		TCPPort  uint16 `json:"tcp_port"`
		QUICPort uint16 `json:"quic_port"`
	}{
		{Host: "carrier.edge.example.test", TCPPort: 17443},
		{Host: "carrier.edge.example.test", TCPPort: 17443, QUICPort: 17443},
		{Host: " carrier.edge.example.test", TCPPort: 17443, QUICPort: 17444},
	} {
		if err := validateCarrierEndpoint(invalid); err == nil {
			t.Fatalf("invalid carrier endpoint %+v was accepted", invalid)
		}
	}
}

func TestEdgeAssignmentSerializesRevokedAsBoolean(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + time.Now().Format("150405.000000000")
	environment, helper, node := "env_"+suffix, "helper_"+suffix, "node_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id) VALUES ($1,$2)`, environment, "workspace_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch) VALUES ($1,'default','1.0',$2)`, node, "process_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, helper, environment); err != nil {
		t.Fatal(err)
	}
	machineID := seedConnectorTestMachine(t, store, environment)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,edge_pool,edge_node_id,state) VALUES ($1,$2,'default',$3,'pending')`, environment, machineID, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_connector_generations WHERE environment_id=$1`, environment)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_helpers WHERE environment_id=$1`, environment)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_environments WHERE id=$1`, environment)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/edge/assignments/current", strings.NewReader(`{"environment_id":"`+environment+`","machine_id":"`+machineID+`","connector_id":"runtime"}`))
	request.Header.Set("Authorization", "Bearer edge-control-test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewEdgeService(store, "edge-control-test").Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Revoked any `json:"revoked"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Revoked != false {
		t.Fatalf("assignment=%s err=%v", response.Body.String(), err)
	}
}

func TestEdgeHandlerRejectsUnknownAndTrailingJSON(t *testing.T) {
	service := NewEdgeService(nil, "edge-control-credential-01234567890123456789")
	for _, body := range []string{
		`{"edge_node_id":"node","unknown":true}`,
		`{"edge_node_id":"node"}{"edge_node_id":"other"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/edge/routes/desired-state", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer edge-control-credential-01234567890123456789")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d", body, response.Code)
		}
	}
}

func TestEdgeHandlerReturnsCompleteCanonicalSnapshotForProcess(t *testing.T) {
	store := openControlPlaneTestDB(t)
	service := NewEdgeService(store, "edge-control-credential-01234567890123456789")
	request := httptest.NewRequest(http.MethodPost, "/v1/edge/routes/desired-state", strings.NewReader(`{"edge_node_id":"edge_01","process_epoch":"epoch_0001"}`))
	request.Header.Set("Authorization", "Bearer edge-control-credential-01234567890123456789")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("canonical snapshot status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Complete bool             `json:"complete"`
		Routes   []map[string]any `json:"routes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Complete || envelope.Routes == nil || len(envelope.Routes) != 0 {
		t.Fatalf("canonical snapshot = %+v", envelope)
	}
}

func TestLegacyEdgeRouteJSONProjectsAppliedReadiness(t *testing.T) {
	got := legacyEdgeRouteJSON(dbsqlc.ListControlRoutesForNodeRow{
		RouteID: "rte_runtime_01", RouteRevision: 2, EnvironmentID: "env_01",
		ConnectorID: "runtime", ConnectorGeneration: 4,
		EdgeNodeID: sql.NullString{String: "edge_01", Valid: true},
		Kind:       "runtime_https_wss", PublicHost: "machine.runtime.example.test",
		TargetHost: "127.0.0.1", TargetPort: 38080,
		PreviewState: "ready", PreviewReason: "",
	})
	if got["preview_state"] != "ready" || got["preview_reason"] != "" {
		t.Fatalf("readiness projection = %#v", got)
	}
	target, ok := got["target"].(map[string]any)
	if !ok || target["host"] != "127.0.0.1" || target["port"] != int32(38080) {
		t.Fatalf("target projection = %#v", got["target"])
	}
}
