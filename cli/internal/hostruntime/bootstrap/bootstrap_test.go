package bootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
)

var testPublicIdentityKey = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))

func TestDashboardTokenPairingAndMaterialExchange(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	var pairingCalls, materialCalls int
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/machines/pairings":
			pairingCalls++
			var body map[string]any
			if json.NewDecoder(request.Body).Decode(&body) != nil || body["enrollment_token"] != "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP" || body["platform"] != runtime.GOOS || body["architecture"] != runtime.GOARCH || body["workspace_root"] != workspace || body["alias"] != "studio" || body["display_name"] != nil || body["ssh_user"] != "developer" || body["ssh_port"] != float64(22) {
				t.Fatalf("pairing body=%v", body)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": Pairing{ID: "cmp_1", UserCode: "ABCD1234", ExpiresAt: expires}})
		case "/v1/machines/pairings/installation":
			materialCalls++
			if materialCalls == 1 {
				writer.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"code": "user_machine_approval_pending", "message": "Machine approval is pending."}})
				return
			}
			manifest := descriptor(server.URL, "0.0.0-development")
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": Material{Schema: "paperboat.machine-installation/v1", UserMachineID: "um_1", PairingID: "ume_1", EnvironmentID: "env_1", ControlURL: server.URL, HelperID: "helper_1", EnrollmentID: "enroll_1", EnrollmentCredential: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", ExpiresAt: expires, Artifact: &manifest, HelperListenAddress: "127.0.0.1:38080", InstallationGeneration: 1, ClientSession: &ClientSession{Schema: "paperboat.cli-session/v1", SessionID: "cls_1", AccessToken: "access-012345678901234567890123456789", RefreshToken: "refresh-012345678901234567890123456789", TokenType: "Bearer", ExpiresIn: 3600}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, SSHUser: "developer", SSHPort: 22, HTTP: server.Client()}
	pairing, err := CreatePairing(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	material, err := WaitForMaterial(context.Background(), config, pairing.ExpiresAt, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if pairingCalls != 1 || materialCalls != 2 || material.EnvironmentID != "env_1" {
		t.Fatalf("pairing=%d material=%d result=%+v", pairingCalls, materialCalls, material)
	}
}

func TestIdentityPairingDoesNotRequireEnrollmentToken(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if json.NewDecoder(request.Body).Decode(&body) != nil || body["enrollment_token"] != "" || body["public_identity_key"] != testPublicIdentityKey {
			t.Fatalf("pairing body=%v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": Pairing{ID: "cmp_identity", UserCode: "EFGH5678", ExpiresAt: expires}})
	}))
	defer server.Close()

	config := Config{ServerURL: server.URL, Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	if _, err := CreatePairing(context.Background(), config); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardEnrollmentTokenLengthContract(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if json.NewDecoder(request.Body).Decode(&body) != nil || body["enrollment_token"] != "8GXDIGUWR4E6YIGL0D6X0H3FNA" {
			t.Fatalf("pairing body=%v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": Pairing{ID: "cmp_dashboard", UserCode: "ABCD1234", ExpiresAt: expires}})
	}))
	defer server.Close()

	config := Config{ServerURL: server.URL, EnrollmentToken: "8GXDIGUWR4E6YIGL0D6X0H3FNA", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	if _, err := CreatePairing(context.Background(), config); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePairingErrorKeepsStatusAndHidesServerResponse(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serverBody := `{"error":{"code":"invalid_user_machine_pairing","message":"BOOTSTRAP_RESPONSE_SECRET_DO_NOT_EXPORT","request_id":"req_bootstrap_123","details":{"reason":"artifact unavailable"}}}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/machines/pairings" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(serverBody))
	}))
	defer server.Close()

	config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	_, err = CreatePairing(context.Background(), config)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), "invalid_user_machine_pairing") || strings.Contains(err.Error(), "BOOTSTRAP_RESPONSE_SECRET_DO_NOT_EXPORT") || strings.Contains(err.Error(), "req_bootstrap_123") {
		t.Fatalf("server response escaped into error: %v", err)
	}
	if !errorreport.HTTPAttemptObserved(err) {
		t.Fatal("owned HTTP status attempt marker was lost")
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-cli", "machine_pairing", "command", "unexpected_cli_failure", err)
	if fault.Stage != "control_request" || fault.Code != "control_request_failed" || fault.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("projected fault = %+v", fault)
	}
}

func TestCreatePairingRetriesOnlyBeforeRequestIsWritten(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": Pairing{ID: "cmp_retry", UserCode: "ABCD1234", ExpiresAt: expires}})
	}))
	defer server.Close()
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		return server.Client().Transport.RoundTrip(request)
	})
	config := Config{
		ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP",
		Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP",
		PublicIdentityKey: testPublicIdentityKey, HTTP: &http.Client{Transport: transport, Timeout: 2 * time.Second},
	}
	pairing, err := CreatePairing(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || pairing.ID != "cmp_retry" {
		t.Fatalf("calls=%d pairing=%#v", calls, pairing)
	}
}

func TestCreatePairingDoesNotRetryAfterRequestIsWritten(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		trace := httptrace.ContextClientTrace(request.Context())
		if trace == nil || trace.WroteRequest == nil {
			t.Fatal("bootstrap request did not install a write trace")
		}
		trace.WroteRequest(httptrace.WroteRequestInfo{})
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	})
	config := Config{
		ServerURL: "https://api.example.test", EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP",
		Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP",
		PublicIdentityKey: testPublicIdentityKey, HTTP: &http.Client{Transport: transport, Timeout: 2 * time.Second},
	}
	if _, err := CreatePairing(context.Background(), config); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestBootstrapServerErrorDoesNotExposeLargeResponse(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"code":"BOOTSTRAP_RESPONSE_SECRET_DO_NOT_EXPORT","message":"` + strings.Repeat("private-response-content", maxBootstrapResponseBody) + `"}}`))
	}))
	defer server.Close()

	config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	_, err = CreatePairing(context.Background(), config)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), "BOOTSTRAP_RESPONSE_SECRET_DO_NOT_EXPORT") || strings.Contains(err.Error(), "private-response-content") || strings.Contains(err.Error(), "<truncated>") {
		t.Fatalf("response body escaped into error: %v", err)
	}
	if !errorreport.HTTPAttemptObserved(err) {
		t.Fatal("oversized error response lost the owned HTTP attempt marker")
	}
	if len(err.Error()) > 256 {
		t.Fatalf("error length = %d, want a short static message", len(err.Error()))
	}
}

func TestWaitForMaterialRetriesTransientResponseReadAndKeepsCause(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		manifest := descriptor(server.URL, "0.0.0-development")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": Material{Schema: "paperboat.machine-installation/v1", UserMachineID: "um_1", PairingID: "ume_1", EnvironmentID: "env_1", ControlURL: server.URL, HelperID: "helper_1", EnrollmentID: "enroll_1", EnrollmentCredential: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", ExpiresAt: expires, Artifact: &manifest, HelperListenAddress: "127.0.0.1:38080", InstallationGeneration: 1, ClientSession: &ClientSession{Schema: "paperboat.cli-session/v1", SessionID: "cls_1", AccessToken: "access-012345678901234567890123456789", RefreshToken: "refresh-012345678901234567890123456789", TokenType: "Bearer", ExpiresIn: 3600}}})
	}))
	defer server.Close()
	base := server.Client().Transport
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		response, err := base.RoundTrip(request)
		if err == nil && calls == 1 {
			_ = response.Body.Close()
			response.Body = &bootstrapTestBody{reader: bootstrapErrorReader{err: io.ErrUnexpectedEOF}}
		}
		return response, err
	})}
	config := Config{ServerURL: server.URL, Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: client}
	material, err := WaitForMaterial(context.Background(), config, expires, time.Millisecond)
	if err != nil || calls != 2 || material.EnvironmentID != "env_1" {
		t.Fatalf("requests=%d material=%+v err=%v", calls, material, err)
	}
}

func TestWaitForMaterialDoesNotRetryMixedTransientAndSubstantiveResponseFailure(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	substantive := errors.New("independent response failure")
	calls := 0
	config := Config{ServerURL: "https://api.example.test", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &bootstrapTestBody{reader: bootstrapErrorReader{err: errors.Join(io.ErrUnexpectedEOF, substantive)}}, Request: request}, nil
	})}}
	_, err = WaitForMaterial(context.Background(), config, time.Now().UTC().Add(time.Minute), time.Millisecond)
	if calls != 1 || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, substantive) {
		t.Fatalf("requests=%d error=%v", calls, err)
	}
}

func TestWaitForMaterialDoesNotRetryApprovalWithIndependentCloseFailure(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("independent response close failure")
	calls := 0
	config := Config{ServerURL: "https://api.example.test", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := `{"error":{"code":"machine_approval_pending","message":"not for logs"}}`
		return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: &bootstrapTestBody{reader: strings.NewReader(body), closeErr: closeFailure}, Request: request}, nil
	})}}
	_, err = WaitForMaterial(context.Background(), config, time.Now().UTC().Add(time.Minute), time.Millisecond)
	if calls != 1 || !errors.Is(err, ErrApprovalPending) || !errors.Is(err, closeFailure) {
		t.Fatalf("requests=%d error=%v", calls, err)
	}
	if strings.Contains(err.Error(), "not for logs") || strings.Contains(err.Error(), closeFailure.Error()) {
		t.Fatalf("untrusted data escaped in error: %v", err)
	}
}

func TestBootstrapRetryClassifiersRejectCycles(t *testing.T) {
	cycle := &bootstrapCycleFailure{}
	if transientBootstrapError(cycle) || bootstrapOnlyCause(cycle, context.Canceled) || bootstrapOnlyOutcome(cycle, ErrApprovalPending) {
		t.Fatal("cyclic error was classified as a retryable or expected outcome")
	}
}

func TestBootstrapRetryClassifiersRejectTimeoutWrappingMixedCauses(t *testing.T) {
	failure := &net.OpError{Op: "dial", Net: "tcp", Err: errors.Join(os.ErrDeadlineExceeded, errors.New("independent bootstrap failure"))}
	if transientBootstrapError(failure) {
		t.Fatal("timeout wrapper hid an independent cause")
	}
}

type bootstrapTestBody struct {
	reader   io.Reader
	closeErr error
}

func (body *bootstrapTestBody) Read(value []byte) (int, error) { return body.reader.Read(value) }
func (body *bootstrapTestBody) Close() error                   { return body.closeErr }

type bootstrapErrorReader struct{ err error }

func (reader bootstrapErrorReader) Read([]byte) (int, error) { return 0, reader.err }

type bootstrapCycleFailure struct{}

func (*bootstrapCycleFailure) Error() string         { return "cyclic bootstrap failure" }
func (failure *bootstrapCycleFailure) Unwrap() error { return failure }

func TestWaitForMaterialStopsOnTerminalServerErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{name: "denied", status: http.StatusForbidden, code: "machine_pairing_denied", want: ErrPairingDenied},
		{name: "expired", status: http.StatusGone, code: "machine_pairing_expired", want: ErrPairingExpired},
		{name: "unavailable", status: http.StatusGone, code: "machine_installation_unavailable", want: ErrInstallationUnavailable},
		{name: "server denied", status: http.StatusForbidden, code: "user_machine_pairing_denied", want: ErrPairingDenied},
		{name: "server expired", status: http.StatusGone, code: "user_machine_pairing_expired", want: ErrPairingExpired},
		{name: "server unavailable", status: http.StatusGone, code: "user_machine_installation_unavailable", want: ErrInstallationUnavailable},
		{name: "server failure", status: http.StatusInternalServerError, code: "internal_error", want: ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls++
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"code": test.code, "message": "test"}})
			}))
			defer server.Close()
			config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
			_, err = WaitForMaterial(context.Background(), config, time.Now().UTC().Add(time.Minute), time.Millisecond)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
		})
	}
}

func TestWaitForMaterialToleratesTransientNetworkErrors(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	calls := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			writer.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"code": "user_machine_approval_pending", "message": "Machine approval is pending."}})
		case 2:
			panic(http.ErrAbortHandler)
		default:
			manifest := descriptor(server.URL, "0.0.0-development")
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": Material{Schema: "paperboat.machine-installation/v1", UserMachineID: "um_1", PairingID: "ume_1", EnvironmentID: "env_1", ControlURL: server.URL, HelperID: "helper_1", EnrollmentID: "enroll_1", EnrollmentCredential: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", ExpiresAt: expires, Artifact: &manifest, HelperListenAddress: "127.0.0.1:38080", InstallationGeneration: 1, ClientSession: &ClientSession{Schema: "paperboat.cli-session/v1", SessionID: "cls_1", AccessToken: "access-012345678901234567890123456789", RefreshToken: "refresh-012345678901234567890123456789", TokenType: "Bearer", ExpiresIn: 3600}}})
		}
	}))
	defer server.Close()
	config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	material, err := WaitForMaterial(context.Background(), config, expires, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || material.EnvironmentID != "env_1" {
		t.Fatalf("requests=%d material=%+v", calls, material)
	}
}

func TestWaitForMaterialExpiresAfterTransientErrors(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()
	config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	_, err = WaitForMaterial(context.Background(), config, time.Now().UTC().Add(40*time.Millisecond), time.Millisecond)
	if !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("error = %v, want %v", err, ErrPairingExpired)
	}
}

func TestRecoverMaterialIgnoresLocalPairingExpiry(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	materialExpiry := time.Now().UTC().Add(time.Hour)
	calls := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		var body struct {
			Verifier          string `json:"verifier"`
			PublicIdentityKey string `json:"public_identity_key"`
			RuntimeEnrolled   bool   `json:"runtime_enrolled"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil || body.Verifier != "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP" || body.PublicIdentityKey != testPublicIdentityKey || !body.RuntimeEnrolled {
			t.Fatalf("recovery body=%v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		manifest := descriptor(server.URL, "0.0.0-development")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": Material{
			Schema: "paperboat.machine-installation/v1", UserMachineID: "um_1", PairingID: "ume_1", EnvironmentID: "env_1", ControlURL: server.URL,
			HelperID: "helper_1", EnrollmentID: "enroll_1", EnrollmentCredential: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", ExpiresAt: materialExpiry,
			Artifact: &manifest, HelperListenAddress: "127.0.0.1:38080", InstallationGeneration: 1, ClientSession: &ClientSession{Schema: "paperboat.cli-session/v1", SessionID: "cls_1", AccessToken: "access-012345678901234567890123456789", RefreshToken: "refresh-012345678901234567890123456789", TokenType: "Bearer", ExpiresIn: 3600},
		}})
	}))
	defer server.Close()
	config := Config{ServerURL: server.URL, EnrollmentToken: "", Alias: "Studio", WorkspaceRoot: workspace, Verifier: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", PublicIdentityKey: testPublicIdentityKey, HTTP: server.Client()}
	material, err := RecoverMaterial(context.Background(), config, true)
	if err != nil || calls != 1 || !material.ExpiresAt.Equal(materialExpiry) {
		t.Fatalf("material=%+v requests=%d err=%v", material, calls, err)
	}
}

func TestValidateWorkspaceRejectsSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if ValidateWorkspace(link) == nil {
		t.Fatal("expected symlink workspace to be rejected")
	}
}

func TestValidateWorkspacePreservesFilesystemCause(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-workspace")
	err := ValidateWorkspace(missing)
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace error=%v, want invalid input and original filesystem cause", err)
	}
}

func TestValidateMaterialAcceptsClientCLISetup(t *testing.T) {
	material := Material{
		Schema: "paperboat.machine-installation/v1", UserMachineID: "um_1", PairingID: "ume_1",
		EnvironmentID: "env_1", ControlURL: "https://example.test", HelperID: "helper_1", EnrollmentID: "enroll_1",
		EnrollmentCredential: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOP", ExpiresAt: time.Now().UTC().Add(time.Minute),
		Artifact:            &ArtifactTarget{Schema: ArtifactTargetSchemaV1, Kind: ArtifactKindPB, Version: "2026.01.01.0", Platform: runtime.GOOS, Architecture: runtime.GOARCH, RepositoryURL: "https://example.test/tuf", TargetPath: releaseindex.AssetName(runtime.GOOS, runtime.GOARCH)},
		HelperListenAddress: "127.0.0.1:38080", InstallationGeneration: 1, ClientSession: &ClientSession{Schema: "paperboat.cli-session/v1", SessionID: "cls_1", AccessToken: "access-012345678901234567890123456789", RefreshToken: "refresh-012345678901234567890123456789", TokenType: "Bearer", ExpiresIn: 3600},
	}
	if err := validateMaterial(material); err != nil {
		t.Fatalf("client material rejected: %v", err)
	}
}
