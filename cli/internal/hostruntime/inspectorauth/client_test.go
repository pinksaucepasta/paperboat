package inspectorauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
)

type stubSource struct {
	token    string
	proof    []byte
	tokenErr error
	proofErr error
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (failingBody) Close() error               { return nil }

func (s stubSource) Token(context.Context) (string, error) { return s.token, s.tokenErr }
func (s stubSource) Proof(_ context.Context, _, _, _ string, _ []byte) ([]byte, error) {
	return s.proof, s.proofErr
}

func decisionPayload() map[string]any {
	now := time.Now().UTC()
	return map[string]any{"data": map[string]any{
		"account_id": "mate_01", "owner_account_id": "owner_01",
		"resource_kind": "tunnel", "resource_id": "tun_01", "route_id": "rte_01",
		"resource_generation": 3, "route_generation": 3, "target_generation": 3,
		"credential_id": "iac_01",
		"issued_at":     now.Format(time.RFC3339Nano), "expires_at": now.Add(10 * time.Second).Format(time.RFC3339Nano),
	}}
}

func TestAuthorizeSendsMachineProofAndMapsDecision(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotHeaders = request.Header.Clone()
		body, _ := io.ReadAll(request.Body)
		gotBody = body
		if request.URL.Path != "/v1/inspector/authorize" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(decisionPayload())
	}))
	defer server.Close()

	authorize, err := Config{BaseURL: server.URL, Source: stubSource{token: "machine-token", proof: []byte("machine-proof")}}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	decision, err := authorize(context.Background(), "grant-token", "tunnel", "tun_01", "rte_01", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if decision.CredentialID != "iac_01" || decision.Principal != "mate_01" || decision.Owner != "owner_01" || decision.RouteGeneration != 3 {
		t.Fatalf("decision = %+v", decision)
	}
	if gotHeaders.Get("Authorization") != "Bearer machine-token" || gotHeaders.Get("X-Paperboat-Machine-Identity") != "machine-token" {
		t.Fatalf("machine identity headers = %v", gotHeaders)
	}
	if gotHeaders.Get("X-Paperboat-Machine-Proof") != base64.RawURLEncoding.EncodeToString([]byte("machine-proof")) || gotHeaders.Get("Idempotency-Key") == "" {
		t.Fatalf("proof headers = %v", gotHeaders)
	}
	var decoded struct {
		CredentialToken string `json:"credential_token"`
		Action          string `json:"action"`
	}
	if err := json.Unmarshal(gotBody, &decoded); err != nil || decoded.CredentialToken != "grant-token" || decoded.Action != "inspect" {
		t.Fatalf("body = %s err=%v", gotBody, err)
	}
}

func TestAuthorizeMapsDenialsAndOutages(t *testing.T) {
	for status, want := range map[int]error{400: inspectorapi.ErrInvalid, 403: inspectorapi.ErrDenied, 404: inspectorapi.ErrDenied, 410: inspectorapi.ErrDenied, 401: inspectorapi.ErrUpstream, 500: inspectorapi.ErrUpstream} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, `{}`)
		}))
		authorize, err := Config{BaseURL: server.URL, Source: stubSource{token: "t", proof: []byte("p")}}.AuthorizeFunc()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authorize(context.Background(), "grant", "tunnel", "tun_01", "rte_01", "inspect"); !errors.Is(err, want) {
			t.Fatalf("status %d = %v, want %v", status, err, want)
		}
		server.Close()
	}
	// A success response with a mismatched binding is an authority failure,
	// not a user denial that could trigger fresh issuance.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		payload := decisionPayload()
		payload["data"].(map[string]any)["resource_id"] = "other"
		_ = json.NewEncoder(writer).Encode(payload)
	}))
	defer server.Close()
	authorize, err := Config{BaseURL: server.URL, Source: stubSource{token: "t", proof: []byte("p")}}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorize(context.Background(), "grant", "tunnel", "tun_01", "rte_01", "inspect"); !errors.Is(err, inspectorapi.ErrUpstream) || errors.Is(err, inspectorapi.ErrDenied) {
		t.Fatalf("mismatched decision = %v, want authority failure", err)
	}
	// Grant material never appears in error text.
	if _, err := authorize(context.Background(), "grant", "tunnel", "tun_01", "rte_01", "bogus"); err == nil || strings.Contains(err.Error(), "grant") {
		t.Fatalf("input validation leaked grant: %v", err)
	}
}

func TestAuthorizePreservesLocalFailureCausesWithoutCredentialText(t *testing.T) {
	secret := errors.New("private machine identity token text")
	authorize, err := Config{BaseURL: "https://control.example", Source: stubSource{tokenErr: secret}}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorize(context.Background(), "private grant token", "tunnel", "tun_01", "rte_01", "inspect")
	if !errors.Is(err, inspectorapi.ErrUpstream) || !errors.Is(err, secret) {
		t.Fatalf("local cause was lost: %v", err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("local credential text escaped in error: %q", err.Error())
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "inspector_authorization", peerAuthorityStage, peerAuthorityCode, err)
	if fault.Stage != controlRequestStage || fault.Code != controlRequestCode {
		t.Fatalf("local request phase was lost: %#v", fault)
	}
}

func TestAuthorizeRetainsOwnedHTTPStatusWithoutResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, "private response payload")
	}))
	defer server.Close()
	authorize, err := Config{BaseURL: server.URL, Source: stubSource{token: "machine-token", proof: []byte("machine-proof")}}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorize(context.Background(), "grant-token", "tunnel", "tun_01", "rte_01", "inspect")
	if !errors.Is(err, inspectorapi.ErrUpstream) || !errorreport.HTTPAttemptObserved(err) || strings.Contains(err.Error(), "private response payload") {
		t.Fatalf("unsafe or unowned status error: %v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "inspector_authorization", peerAuthorityStage, peerAuthorityCode, err)
	if fault.Stage != controlRequestStage || fault.Code != controlRequestCode || fault.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("HTTP phase/status was lost: %#v", fault)
	}
}

func TestAuthorizeKeepsResponseReadFailureObservable(t *testing.T) {
	readFailure := errors.New("private response read detail")
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: failingBody{err: readFailure}, Header: make(http.Header), Request: request}, nil
	})}
	authorize, err := Config{BaseURL: "https://control.example", Source: stubSource{token: "machine-token", proof: []byte("machine-proof")}, Client: client}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorize(context.Background(), "grant-token", "tunnel", "tun_01", "rte_01", "inspect")
	if !errors.Is(err, inspectorapi.ErrUpstream) || !errors.Is(err, readFailure) || errorreport.HTTPAttemptObserved(err) || strings.Contains(err.Error(), "private response read detail") {
		t.Fatalf("response read failure was lost or exposed: %v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "inspector_authorization", peerAuthorityStage, peerAuthorityCode, err)
	if fault.Stage != controlRequestStage || fault.Code != controlRequestCode {
		t.Fatalf("response read phase was lost: %#v", fault)
	}
}

func TestAuthorizeKeepsMalformedResponseCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "{private malformed payload")
	}))
	defer server.Close()
	authorize, err := Config{BaseURL: server.URL, Source: stubSource{token: "machine-token", proof: []byte("machine-proof")}}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorize(context.Background(), "grant-token", "tunnel", "tun_01", "rte_01", "inspect")
	var syntaxError *json.SyntaxError
	if !errors.Is(err, inspectorapi.ErrUpstream) || !errors.As(err, &syntaxError) || strings.Contains(err.Error(), "private malformed payload") {
		t.Fatalf("malformed response cause was lost or exposed: %v", err)
	}
}

func TestAuthorizeTreatsMalformedSuccessfulDecisionAsAuthorityFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		payload := decisionPayload()
		payload["data"].(map[string]any)["resource_id"] = "different_resource"
		_ = json.NewEncoder(writer).Encode(payload)
	}))
	defer server.Close()
	authorize, err := Config{BaseURL: server.URL, Source: stubSource{token: "machine-token", proof: []byte("machine-proof")}}.AuthorizeFunc()
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorize(context.Background(), "grant-token", "tunnel", "tun_01", "rte_01", "inspect")
	if !errors.Is(err, inspectorapi.ErrUpstream) || errors.Is(err, inspectorapi.ErrDenied) || strings.Contains(err.Error(), "different_resource") {
		t.Fatalf("invalid authority decision was not safely rejected: %v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "inspector_authorization", controlRequestStage, controlRequestCode, err)
	if fault.Stage != peerAuthorityStage || fault.Code != peerAuthorityCode {
		t.Fatalf("authority phase was lost: %#v", fault)
	}
}
