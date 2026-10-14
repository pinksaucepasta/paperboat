package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type identityTokenFunc func(context.Context) (string, error)

func (f identityTokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

type helperProofFunc func(context.Context, string, string, string, []byte) ([]byte, error)

func (f helperProofFunc) Proof(ctx context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	return f(ctx, operationID, method, path, body)
}

type credentialVerifierFunc func(context.Context, string, auth.Policy) (auth.Claims, error)

func (f credentialVerifierFunc) Verify(ctx context.Context, token string, policy auth.Policy) (auth.Claims, error) {
	return f(ctx, token, policy)
}

type httpRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f httpRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func admissionSourceFor(t *testing.T, responseBody func(admissionRequest) string, verifier CredentialVerifier) *HTTPSAdmissionSource {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var operation atomic.Uint64
	source, err := NewHTTPSAdmissionSource(AdmissionSourceConfig{
		Endpoint: "https://api.test/v1/connectors/admission", AllowedHosts: []string{"api.test"}, Tokens: identityTokenFunc(func(context.Context) (string, error) { return "helper-identity", nil }), Proofs: helperProofFunc(func(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
			if operationID == "" || method != http.MethodPost || path != "/v1/connectors/admission" || len(body) == 0 {
				return nil, errors.New("incorrect proof input")
			}
			return []byte("signed-helper-proof"), nil
		}), Verifier: verifier,
		Clock: fixedClock{now}, Issuer: "https://api.test", EnvironmentID: "env", MachineID: "helper", ConnectorID: "runtime", EdgePool: "default",
		OperationID: func() (string, error) { return "op_admit_000" + string(rune('0'+operation.Add(1))), nil },
		Transport: httpRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "Bearer helper-identity" {
				return nil, errors.New("missing helper identity")
			}
			proof, err := base64.RawURLEncoding.DecodeString(request.Header.Get("X-Paperboat-Machine-Proof"))
			if err != nil || string(proof) != "signed-helper-proof" {
				return nil, errors.New("missing helper proof")
			}
			var input admissionRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody(input))), Request: request}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func validAdmissionResponse(input admissionRequest) string {
	encoded, _ := json.Marshal(admissionResponse{OperationID: input.OperationID, EnvironmentID: input.EnvironmentID, MachineID: input.MachineID, ConnectorID: input.ConnectorID, Generation: 3, EdgePool: input.EdgePool, EdgeNodeID: "edge_1", RelayHTTPEndpoint: "https://relay.test", EdgeEndpoint: EdgeEndpoint{Host: "edge.test", Port: 7000}, Routes: []RouteHandoff{{RouteID: "route_1", Revision: 1, Kind: "runtime_https_wss", PublicHost: "helper.test", ProxyName: "helper_1", LocalTarget: RouteTarget{Host: "127.0.0.1", Port: 8080}}}, ProtocolVersion: "1.0", Capabilities: []string{"terminal.v1"}, Credential: "test-only-connector-admission-credential", FileTransferPolicy: testFileTransferPolicy()})
	return string(encoded)
}

func testFileTransferPolicy() auth.FileTransferPolicy {
	return auth.FileTransferPolicy{Revision: "file-transfer-v1", MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20, MaxConcurrentTransfers: 2, RetentionSeconds: 604800, DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}
}

func TestHTTPSAdmissionSourceVerifiesExactBindingsAndReturnsCredential(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	source := admissionSourceFor(t, validAdmissionResponse, credentialVerifierFunc(func(_ context.Context, token string, policy auth.Policy) (auth.Claims, error) {
		if token != "test-only-connector-admission-credential" || policy.Issuer != "https://api.test" || policy.Audience != "paperboat-edge" || policy.CredentialClass != "connector_admission" || policy.EnvironmentID != "env" || policy.MachineID != "helper" || policy.ConnectorGeneration != 3 || policy.EdgePool != "default" || policy.EdgeNodeID != "edge_1" || !policy.SingleUse {
			return auth.Claims{}, errors.New("incorrect policy")
		}
		transferPolicy := testFileTransferPolicy()
		routes := []RouteHandoff{{RouteID: "route_1", Revision: 1, Kind: "runtime_https_wss", PublicHost: "helper.test", ProxyName: "helper_1", LocalTarget: RouteTarget{Host: "127.0.0.1", Port: 8080}}}
		return auth.Claims{JTI: "jti_admit_0001", EdgePool: "default", EdgeNodeID: "edge_1", RouteBinding: connectorRouteBinding(routes), ExpiresAt: now.Add(time.Minute).Unix(), FileTransferPolicy: &transferPolicy}, nil
	}))
	admission, err := source.Admission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if admission.Credential != "test-only-connector-admission-credential" || admission.Generation != 3 || admission.JTI != "jti_admit_0001" || admission.RelayHTTPEndpoint != "https://relay.test" {
		t.Fatalf("admission=%#v", admission)
	}
}

func TestSupervisorDoesNotDuplicateObservedAdmissionTransportFailure(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var requests atomic.Uint64
	secret := "private admission token and endpoint"
	source := admissionSourceFor(t, validAdmissionResponse, credentialVerifierFunc(func(_ context.Context, _ string, _ auth.Policy) (auth.Claims, error) {
		policy := testFileTransferPolicy()
		routes := []RouteHandoff{{RouteID: "route_1", Revision: 1, Kind: "runtime_https_wss", PublicHost: "helper.test", ProxyName: "helper_1", LocalTarget: RouteTarget{Host: "127.0.0.1", Port: 8080}}}
		return auth.Claims{JTI: "jti_admit_0001", EdgePool: "default", EdgeNodeID: "edge_1", RouteBinding: connectorRouteBinding(routes), ExpiresAt: now.Add(time.Minute).Unix(), FileTransferPolicy: &policy}, nil
	}))
	source.client.Transport = errorreport.TransportOperation(httpRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return nil, errors.New(secret)
		}
		var input admissionRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(validAdmissionResponse(input))), Request: request}, nil
	}), source.endpoint.String(), "connector_admission")

	faults := make(chan errorreport.Fault, 8)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	network := &recoveringDialer{}
	manager := manager(t, network, now)
	supervisor, err := NewSupervisor(SupervisorConfig{Manager: manager, Admissions: source, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Waiter: &recordingWaiter{}})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	if err := supervisor.Start(supportref.WithContext(context.Background(), reference)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !manager.Status().Connected {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("connector did not recover after admission transport failure: status=%+v", manager.Status())
		}
	}
	select {
	case fault := <-faults:
		if fault.Operation != "connector_admission" || fault.Stage != "control_request" || fault.Code != "control_request_failed" || fault.SupportReference != reference {
			t.Fatalf("unexpected admission transport fault: %+v", fault)
		}
		if strings.Contains(strings.Join(fault.ErrorChain, ","), secret) || strings.Contains(fault.Cause, secret) || strings.Contains(fault.ErrorType, secret) {
			t.Fatalf("raw admission transport detail escaped: %+v", fault)
		}
	case <-time.After(time.Second):
		t.Fatal("admission transport failure was not observed")
	}
	select {
	case duplicate := <-faults:
		t.Fatalf("supervisor duplicated admission transport failure: %+v", duplicate)
	case <-time.After(25 * time.Millisecond):
	}
	if requests.Load() < 2 {
		t.Fatalf("admission source did not recover; requests=%d", requests.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRelayHTTPEndpointValidation(t *testing.T) {
	for _, value := range []string{"https://relay.test", "https://relay.test:443"} {
		if !validRelayHTTPEndpoint(value) {
			t.Fatalf("valid endpoint rejected: %q", value)
		}
	}
	for _, value := range []string{"", "http://relay.test", "https://user@relay.test", "https://relay.test/path", "https://relay.test?query=1", "https://relay.test/#fragment"} {
		if validRelayHTTPEndpoint(value) {
			t.Fatalf("invalid endpoint accepted: %q", value)
		}
	}
}

func TestHTTPSAdmissionSourceRejectsSignedPolicyMismatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	source := admissionSourceFor(t, validAdmissionResponse, credentialVerifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) {
		policy := testFileTransferPolicy()
		policy.MaxFileBytes--
		return auth.Claims{JTI: "jti_admit_0001", EdgePool: "default", EdgeNodeID: "edge_1", ExpiresAt: now.Add(time.Minute).Unix(), FileTransferPolicy: &policy}, nil
	}))
	if _, err := source.Admission(context.Background()); !errors.Is(err, ErrAdmissionSourceInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPSAdmissionSourceRejectsMalformedCrossBindingAndReplay(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	verifier := credentialVerifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) {
		policy := testFileTransferPolicy()
		return auth.Claims{JTI: "jti", EdgePool: "default", EdgeNodeID: "edge_1", ExpiresAt: now.Add(time.Minute).Unix(), FileTransferPolicy: &policy}, nil
	})
	for name, response := range map[string]func(admissionRequest) string{
		"duplicate": func(input admissionRequest) string {
			valid := validAdmissionResponse(input)
			return strings.Replace(valid, `"environment_id":"env"`, `"environment_id":"env","environment_id":"env"`, 1)
		},
		"environment": func(input admissionRequest) string {
			input.EnvironmentID = "other"
			return validAdmissionResponse(input)
		},
		"capability": func(input admissionRequest) string {
			valid := validAdmissionResponse(input)
			return strings.Replace(valid, `"terminal.v1"`, `"BAD"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := admissionSourceFor(t, response, verifier)
			if _, err := source.Admission(context.Background()); !errors.Is(err, ErrAdmissionSourceInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	replayed := admissionSourceFor(t, validAdmissionResponse, credentialVerifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) {
		return auth.Claims{}, &auth.Error{Code: auth.Replayed}
	}))
	if _, err := replayed.Admission(context.Background()); err == nil {
		t.Fatal("replayed credential accepted")
	}
}

func TestHTTPSAdmissionSourceRejectsUnsafeEndpointAndOversizedResponse(t *testing.T) {
	base := AdmissionSourceConfig{AllowedHosts: []string{"api.test"}, Tokens: identityTokenFunc(func(context.Context) (string, error) { return "token", nil }), Proofs: helperProofFunc(func(context.Context, string, string, string, []byte) ([]byte, error) { return []byte("proof"), nil }), Verifier: credentialVerifierFunc(func(context.Context, string, auth.Policy) (auth.Claims, error) { return auth.Claims{}, nil }), Clock: fixedClock{time.Now()}, Issuer: "https://api.test", EnvironmentID: "env", MachineID: "helper", ConnectorID: "runtime", EdgePool: "default", OperationID: func() (string, error) { return "op_admit_0001", nil }}
	for _, endpoint := range []string{"http://api.test/admit", "https://other.test/admit", "https://user@api.test/admit"} {
		config := base
		config.Endpoint = endpoint
		if _, err := NewHTTPSAdmissionSource(config); !errors.Is(err, ErrAdmissionSourceInvalid) {
			t.Fatalf("endpoint=%s err=%v", endpoint, err)
		}
	}
	source := admissionSourceFor(t, func(admissionRequest) string { return strings.Repeat("x", 65<<10) }, base.Verifier)
	if _, err := source.Admission(context.Background()); !errors.Is(err, ErrAdmissionSourceInvalid) {
		t.Fatalf("oversize err=%v", err)
	}
}
