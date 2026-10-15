package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestMachineEndpointRequestClassifiesUnexpectedRepositoryFailure(t *testing.T) {
	body := `{"operation_id":"operation_machine_01","generation":1,"certificate":"AQ"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/machine-peer-identity", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	verifier := machineProofVerifierFunc(func(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
		return controlplane.MachineRequestClaims{UserID: "account_01", MachineID: "machine_01", InstallationGeneration: 1, OperationID: "operation_machine_01"}, nil
	})
	response := httptest.NewRecorder()
	machineEndpointRequest(machineEndpointRequesterFunc(func(context.Context, peeridentity.MachineRegistrationRequest) (wire.Certificate, error) {
		return wire.Certificate{}, errors.New("database unavailable")
	}), verifier).ServeHTTP(response, request)
	var envelope ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusServiceUnavailable || envelope.Error.Code != "temporarily_unavailable" {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
}

type machineEndpointRequesterFunc func(context.Context, peeridentity.MachineRegistrationRequest) (wire.Certificate, error)

func (f machineEndpointRequesterFunc) RegisterMachine(ctx context.Context, request peeridentity.MachineRegistrationRequest) (wire.Certificate, error) {
	return f(ctx, request)
}

type machineProofVerifierFunc func(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error)

type cliEndpointRequesterFunc func(context.Context, peeridentity.CLIEndpointRequest) (peeridentity.EndpointEnrollmentRequest, error)

func (f cliEndpointRequesterFunc) RequestCLIEndpoint(ctx context.Context, request peeridentity.CLIEndpointRequest) (peeridentity.EndpointEnrollmentRequest, error) {
	return f(ctx, request)
}

func TestCLIEndpointRequestBindsAuthenticatedSessionAndKeys(t *testing.T) {
	quic := bytes.Repeat([]byte{2}, 32)
	var got peeridentity.CLIEndpointRequest
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/endpoint-requests", strings.NewReader(`{"operation_id":"operation_cli_01","endpoint_id":"cli_session_01","generation":1,"quic_public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"}`))
	request.Header.Set("Idempotency-Key", "operation_cli_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	cliEndpointRequest(cliEndpointRequesterFunc(func(_ context.Context, value peeridentity.CLIEndpointRequest) (peeridentity.EndpointEnrollmentRequest, error) {
		got = value
		return peeridentity.EndpointEnrollmentRequest{ID: "per_0123456789abcdef", UserID: value.UserID, EndpointID: value.EndpointID, Generation: value.Generation, Role: wire.RoleCLI, State: "pending", QUICPublicKey: value.QUICPublicKey, CreatedAt: value.Now, ExpiresAt: value.Now.Add(time.Minute)}, nil
	})).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || got.UserID != "account_01" || got.EndpointID != "cli_session_01" || got.Generation != 1 || !bytes.Equal(got.QUICPublicKey[:], quic) {
		t.Fatalf("status=%d request=%+v body=%s", response.Code, got, response.Body.String())
	}
}

func TestCLIEndpointRequestRejectsSessionImpersonation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/endpoint-requests", strings.NewReader(`{"operation_id":"operation_cli_01","endpoint_id":"other_session","generation":1,"quic_public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"}`))
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	cliEndpointRequest(cliEndpointRequesterFunc(func(context.Context, peeridentity.CLIEndpointRequest) (peeridentity.EndpointEnrollmentRequest, error) {
		t.Fatal("requester called")
		return peeridentity.EndpointEnrollmentRequest{}, nil
	})).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCLIEndpointRequestReturnsFulfilledReplayState(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/endpoint-requests", strings.NewReader(`{"operation_id":"operation_cli_01","endpoint_id":"cli_session_01","generation":1,"quic_public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"}`))
	request.Header.Set("Idempotency-Key", "operation_cli_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	cliEndpointRequest(cliEndpointRequesterFunc(func(_ context.Context, value peeridentity.CLIEndpointRequest) (peeridentity.EndpointEnrollmentRequest, error) {
		return peeridentity.EndpointEnrollmentRequest{ID: "per_0123456789abcdef", UserID: value.UserID, EndpointID: value.EndpointID, Generation: value.Generation, Role: wire.RoleCLI, State: "fulfilled", QUICPublicKey: value.QUICPublicKey, CreatedAt: value.Now.Add(-10 * time.Minute), ExpiresAt: value.Now.Add(-5 * time.Minute)}, nil
	})).ServeHTTP(response, request)
	var envelope struct {
		Data wire.EnrollmentDocument `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusCreated || envelope.Data.State != "fulfilled" {
		t.Fatalf("status=%d data=%+v err=%v body=%s", response.Code, envelope.Data, err, response.Body.String())
	}
}

func TestCLIEndpointRequestRequiresMatchingIdempotencyKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "different", header: "operation_cli_other"},
		{name: "surrounding whitespace", header: " operation_cli_01"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/endpoint-requests", strings.NewReader(`{"operation_id":"operation_cli_01","endpoint_id":"cli_session_01","generation":1,"quic_public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"}`))
			if test.header != "" {
				request.Header.Set("Idempotency-Key", test.header)
			}
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
			response := httptest.NewRecorder()
			called := false
			cliEndpointRequest(cliEndpointRequesterFunc(func(context.Context, peeridentity.CLIEndpointRequest) (peeridentity.EndpointEnrollmentRequest, error) {
				called = true
				return peeridentity.EndpointEnrollmentRequest{}, nil
			})).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || called {
				t.Fatalf("status=%d called=%t body=%s", response.Code, called, response.Body.String())
			}
		})
	}
}

func (f machineProofVerifierFunc) VerifyMachineRequest(ctx context.Context, credential string, proof []byte, method, path string, body []byte) (controlplane.MachineRequestClaims, error) {
	return f(ctx, credential, proof, method, path, body)
}

type machineStatusReader struct {
	certificate wire.Certificate
	root        peeridentity.AccountRoot
	err         error
}

func (s machineStatusReader) Get(context.Context, string, string, uint64, time.Time) (wire.Certificate, error) {
	return s.certificate, s.err
}
func (s machineStatusReader) TransportKeys(context.Context, string, time.Time) (peeridentity.AccountRoot, error) {
	return s.root, s.err
}

func TestMachineEndpointStatusReturnsOnlyApprovedPublicAuthority(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rootPublic, _, _ := ed25519.GenerateKey(nil)
	raw := bytes.Repeat([]byte{7}, 172)
	rootFingerprint := sha256.Sum256(rootPublic)
	keyID, _ := peeridentity.KeyID(rootPublic)
	fingerprint := sha256.Sum256(raw)
	reader := machineStatusReader{root: peeridentity.AccountRoot{Keys: []peeridentity.AccountKey{{KeyID: keyID, PublicKey: rootPublic, Fingerprint: rootFingerprint, Generation: 1}}}, certificate: wire.Certificate{AccountID: "account_01", KeyID: keyID, Role: wire.RoleMachine, EndpointID: "machine_01", Generation: 3, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), RootFingerprint: rootFingerprint, Fingerprint: fingerprint, Raw: raw}}
	body := []byte(`{"operation_id":"operation_status_01","generation":3}`)
	verifier := machineProofVerifierFunc(func(_ context.Context, credential string, proof []byte, method, path string, exactBody []byte) (controlplane.MachineRequestClaims, error) {
		if credential != strings.Repeat("t", 32) || string(proof) != "proof" || method != http.MethodPost || path != "/v1/machine-peer-identity/status" || !bytes.Equal(exactBody, body) {
			t.Fatal("machine proof inputs changed")
		}
		return controlplane.MachineRequestClaims{UserID: "account_01", MachineID: "machine_01", InstallationGeneration: 3, OperationID: "operation_status_01"}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/machine-peer-identity/status", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	response := httptest.NewRecorder()
	machineEndpointStatus(reader, verifier).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			State       string                    `json:"state"`
			TrustedKeys []wire.TrustedKeyDocument `json:"trusted_keys"`
			Certificate wire.CertificateDocument  `json:"certificate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.State != "approved" || len(envelope.Data.TrustedKeys) != 1 || envelope.Data.TrustedKeys[0].KeyID != keyID || envelope.Data.Certificate.Certificate != base64.RawURLEncoding.EncodeToString(raw) {
		t.Fatalf("envelope=%+v err=%v", envelope, err)
	}
}

func TestMachineEndpointStatusReportsPendingWithoutCertificate(t *testing.T) {
	body := []byte(`{"operation_id":"operation_status_01","generation":3}`)
	verifier := machineProofVerifierFunc(func(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
		return controlplane.MachineRequestClaims{UserID: "account_01", MachineID: "machine_01", InstallationGeneration: 3, OperationID: "operation_status_01"}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/machine-peer-identity/status", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	response := httptest.NewRecorder()
	machineEndpointStatus(machineStatusReader{err: peeridentity.ErrUnavailable}, verifier).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || strings.Contains(response.Body.String(), "certificate") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMachineEndpointStatusMixedPendingFailureReachesOneSafeOwner(t *testing.T) {
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	const (
		reference        = "support_12345678-1234-4234-8234-123456789abc"
		correlationValue = "correlation_peer_status_01"
		requestIDValue   = "request_peer_status_01"
		privateCause     = "PEER_STATUS_DB_SECRET"
	)
	body := []byte(`{"operation_id":"operation_status_01","generation":3}`)
	verifier := machineProofVerifierFunc(func(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
		return controlplane.MachineRequestClaims{UserID: "account_01", MachineID: "machine_01", InstallationGeneration: 3, OperationID: "operation_status_01"}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/machine-peer-identity/status", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	request.Header.Set("Support-Reference", reference)
	request.Header.Set("Request-Id", requestIDValue)
	request.Header.Set("Correlation-Id", correlationValue)
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	mixed := errors.Join(peeridentity.ErrUnavailable, syscall.EIO, errors.New(privateCause))
	base := machineEndpointStatus(machineStatusReader{err: mixed}, verifier)
	handler := requestID(correlationID(supportMiddleware(reports, slog.New(slog.NewJSONHandler(&logs, nil)), base)))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), privateCause) || strings.Contains(logs.String(), privateCause) {
		t.Fatalf("status=%d response=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) != 0 {
			if len(records) != 1 {
				t.Fatalf("captured %d failures, want one", len(records))
			}
			record := records[0]
			if record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusServiceUnavailable || record.Reference != reference || record.CorrelationID != correlationValue || record.RequestID != requestIDValue || record.Code != "temporarily_unavailable" {
				t.Fatalf("HTTP owner lost the operational cause or request identity: %+v", record)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("HTTP owner did not capture the operational failure")
}

func TestMachineEndpointRequestReadFailurePreservesCauseAndPureCancellationIsQuiet(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "pure cancellation", err: context.Canceled, wantStatus: 0},
		{name: "cancellation with read failure", err: errors.Join(context.Canceled, syscall.EIO, errors.New("MACHINE_BODY_SECRET")), wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/machine-peer-identity")
			request.Method = http.MethodPost
			request.Body = failingMachineEndpointBody{err: test.err}
			response := httptest.NewRecorder()
			machineEndpointRequest(machineEndpointRequesterFunc(func(context.Context, peeridentity.MachineRegistrationRequest) (wire.Certificate, error) {
				t.Fatal("body failure reached registration")
				return wire.Certificate{}, nil
			}), machineProofVerifierFunc(func(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
				t.Fatal("body failure reached proof verification")
				return controlplane.MachineRequestClaims{}, nil
			})).ServeHTTP(response, request)
			if test.wantStatus == 0 {
				if response.Body.Len() != 0 || logs.Len() != 0 {
					t.Fatalf("pure cancellation produced response=%s logs=%s", response.Body.String(), logs.String())
				}
				return
			}
			if response.Code != test.wantStatus || strings.Contains(response.Body.String(), "MACHINE_BODY_SECRET") || strings.Contains(logs.String(), "MACHINE_BODY_SECRET") {
				t.Fatalf("status=%d response=%s logs=%s", response.Code, response.Body.String(), logs.String())
			}
			for _, value := range []string{"system_error", "temporarily_unavailable", reference, `"errno":5`} {
				if !strings.Contains(logs.String(), value) {
					t.Errorf("safe read failure log omitted %q: %s", value, logs.String())
				}
			}
		})
	}
}

type failingMachineEndpointBody struct{ err error }

func (body failingMachineEndpointBody) Read([]byte) (int, error) { return 0, body.err }
func (failingMachineEndpointBody) Close() error                  { return nil }
