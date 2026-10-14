package privateaccess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type accessorHTTPRepo struct {
	machineErr error
	edgeErr    error
}

func (r accessorHTTPRepo) MachineRoutes(context.Context, string, string) ([]AccessorAdmission, error) {
	return nil, r.machineErr
}

func (r accessorHTTPRepo) EdgeAdmissions(context.Context, string, string) ([]AccessorAdmission, error) {
	return nil, r.edgeErr
}

func TestAccessorRoutesSeparateCredentialRejectionFromAuthorityOutage(t *testing.T) {
	ctx, _, logs, reference := privateAccessDiagnosticContext(t)
	private := errors.New("PRIVATE_ACCESSOR_DATABASE_DETAIL")
	outage := errors.Join(syscall.ECONNREFUSED, private)
	claims := controlplane.MachineRequestClaims{UserID: "acct_1", MachineID: "machine_1", InstallationGeneration: 1, OperationID: "operation_1"}

	machineBody, err := json.Marshal(accessorRequest{IdempotencyKey: "operation_1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		verifier error
		repo     error
		want     int
	}{
		{name: "bad machine proof", verifier: controlplane.ErrHelperProof, want: http.StatusUnauthorized},
		{name: "machine verifier database outage", verifier: outage, want: http.StatusServiceUnavailable},
		{name: "bad machine proof joined with I/O failure", verifier: errors.Join(controlplane.ErrHelperProof, syscall.EIO), want: http.StatusServiceUnavailable},
		{name: "machine route read outage", repo: outage, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewAccessorHTTPHandler(accessorHTTPRepo{machineErr: tc.repo}, &fakeMachineRequest{claims: claims, err: tc.verifier}, fakeEdge{})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, MachineRoutesPath, bytes.NewReader(machineBody)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "operation_1")
			request.Header.Set("Authorization", "Bearer machine_control_token")
			request.Header.Set("X-Paperboat-Machine-Identity", "machine_control_token")
			request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "PRIVATE_ACCESSOR_DATABASE_DETAIL") {
				t.Fatal("machine route failure exposed arbitrary error text")
			}
		})
	}

	edgeBody, err := json.Marshal(accessorRequest{EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		verifier error
		repo     error
		want     int
	}{
		{name: "bad edge credential", verifier: previewattachment.ErrUnauthorized, want: http.StatusUnauthorized},
		{name: "edge verifier database outage", verifier: outage, want: http.StatusServiceUnavailable},
		{name: "bad edge credential joined with I/O failure", verifier: errors.Join(previewattachment.ErrUnauthorized, syscall.EIO), want: http.StatusServiceUnavailable},
		{name: "edge admission read outage", repo: outage, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewAccessorHTTPHandler(accessorHTTPRepo{edgeErr: tc.repo}, &fakeMachineRequest{}, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}, err: tc.verifier})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, EdgeAdmissionsPath, bytes.NewReader(edgeBody)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "PRIVATE_ACCESSOR_DATABASE_DETAIL") {
				t.Fatal("edge admission failure exposed arbitrary error text")
			}
		})
	}

	for _, value := range []string{"connection_refused", reference, "correlation_private_access_01"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("accessor route diagnostics omitted %q: %s", value, logs.String())
		}
	}
	if strings.Contains(logs.String(), "PRIVATE_ACCESSOR_DATABASE_DETAIL") {
		t.Fatal("accessor route diagnostics exposed arbitrary error text")
	}
	if got := strings.Count(logs.String(), `"outcome":"failed"`); got != 6 {
		t.Errorf("recorded %d failed operations, want 6: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), `"outcome":"rejected"`); got != 2 {
		t.Errorf("recorded %d expected credential rejections, want 2: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "syscall.Errno") || !strings.Contains(logs.String(), `"http_status":503`) {
		t.Errorf("joined credential and I/O failures lost bounded cause or 503 status: %s", logs.String())
	}
}

func TestAccessorRoutesUseRequestOwnedReporter(t *testing.T) {
	reports, err := supportreport.New(privateAccessReportStore{}, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reports.Close)
	var logs bytes.Buffer
	reports.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	ref := supportreport.NewReference()
	ctx := reports.Context(context.Background(), ref)
	ctx = supportreport.WithFailureIdentity(ctx, "acct_1", "request_002", ref)
	ctx = serverdiagnostics.WithCorrelationID(ctx, "correlation_accessor_02")

	requestBody, err := json.Marshal(accessorRequest{EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001"})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAccessorHTTPHandler(accessorHTTPRepo{edgeErr: syscall.ECONNREFUSED}, &fakeMachineRequest{}, fakeEdge{identity: EdgeIdentity{NodeID: "edge_1", ProcessEpoch: "epoch_001"}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, EdgeAdmissionsPath, bytes.NewReader(requestBody)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
	}
	for _, value := range []string{"connection_refused", ref, "correlation_accessor_02"} {
		if !strings.Contains(logs.String(), value) {
			t.Errorf("request-bound support failure omitted %q: %s", value, logs.String())
		}
	}
}
