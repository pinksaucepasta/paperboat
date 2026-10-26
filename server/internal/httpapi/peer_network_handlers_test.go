package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
)

type peerNetworkStub struct {
	register peersessions.NetworkRegistration
	config   peersessions.NetworkConfigRequest
	err      error
}

func (s *peerNetworkStub) Register(_ context.Context, in peersessions.NetworkRegistration) (peersessions.NetworkRegistrationResult, error) {
	s.register = in
	return peersessions.NetworkRegistrationResult{KeyGeneration: 1, VirtualAddress: "fd7a:115c:a1e0::1"}, s.err
}
func (s *peerNetworkStub) Configuration(_ context.Context, in peersessions.NetworkConfigRequest) (peersessions.NetworkConfigResult, error) {
	s.config = in
	return peersessions.NetworkConfigResult{Configuration: "signed"}, s.err
}

func TestPeerNetworkCLIRegistrationDerivesIdentity(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	disco := bytes.Repeat([]byte{9}, 32)
	fingerprint := bytes.Repeat([]byte{8}, 32)
	body := []byte(`{"operation_id":"operation_123","wireguard_public_key":"` + base64.RawURLEncoding.EncodeToString(key) + `","disco_public_key":"` + base64.RawURLEncoding.EncodeToString(disco) + `","expected_key_generation":0,"quic_certificate_fingerprint":"` + hex.EncodeToString(fingerprint) + `"}`)
	stub := &peerNetworkStub{}
	r := httptest.NewRequest(http.MethodPost, "/v1/peer-network/register", bytes.NewReader(body))
	client := &auth.ClientPrincipal{SessionID: "cli_1"}
	r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "account_1"}, Client: client}))
	w := httptest.NewRecorder()
	peerNetworkRegister(stub).ServeHTTP(w, r)
	if w.Code != http.StatusOK || stub.register.UserID != "account_1" || stub.register.EndpointID != "cli_1" || stub.register.Role != "cli" || !bytes.Equal(stub.register.DiscoPublicKey, disco) {
		t.Fatalf("status=%d registration=%+v body=%s", w.Code, stub.register, w.Body.String())
	}
}

func TestPeerNetworkConfigDerivesIdentityAndBindsLiveScopes(t *testing.T) {
	body := []byte(`{"operation_id":"operation_123","admitted_scopes":[{"resource_kind":"machine_access","resource_id":"access_live"}],"admitted_certificates":[{"endpoint_id":"peer_live","fingerprint":"abababababababababababababababababababababababababababababababab"}]}`)
	stub := &peerNetworkStub{}
	verifier := machineProofVerifierFunc(func(_ context.Context, credential string, proof []byte, method, path string, exact []byte) (controlplane.MachineRequestClaims, error) {
		if credential != "machine-token" || string(proof) != "proof" || method != http.MethodPost || path != "/v1/machine-peer-network/config" || !bytes.Equal(exact, body) {
			t.Fatal("proof boundary changed")
		}
		return controlplane.MachineRequestClaims{UserID: "account_1", MachineID: "machine_1", InstallationGeneration: 3, OperationID: "operation_123"}, nil
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/machine-peer-network/config", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer machine-token")
	r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	w := httptest.NewRecorder()
	machinePeerNetworkConfig(stub, verifier, nil).ServeHTTP(w, r)
	if w.Code != http.StatusOK || stub.config.UserID != "account_1" || stub.config.EndpointID != "machine_1" || len(stub.config.AdmittedCertificates) != 1 || stub.config.AdmittedCertificates[0].EndpointID != "peer_live" || len(stub.config.AdmittedScopes) != 1 || stub.config.AdmittedScopes[0].ResourceID != "access_live" {
		t.Fatalf("status=%d config=%+v", w.Code, stub.config)
	}
	cliRequest := httptest.NewRequest(http.MethodPost, "/v1/peer-network/config", bytes.NewReader(body))
	cliRequest = cliRequest.WithContext(context.WithValue(cliRequest.Context(), authContextKey{}, principal{User: auth.User{ID: "account_2"}, Client: &auth.ClientPrincipal{SessionID: "cli_2"}}))
	w = httptest.NewRecorder()
	peerNetworkConfig(stub).ServeHTTP(w, cliRequest)
	if w.Code != http.StatusOK || stub.config.UserID != "account_2" || stub.config.EndpointID != "cli_2" || len(stub.config.AdmittedCertificates) != 1 || stub.config.AdmittedCertificates[0].EndpointID != "peer_live" || len(stub.config.AdmittedScopes) != 1 || stub.config.AdmittedScopes[0].ResourceKind != "machine_access" || stub.config.AdmittedScopes[0].ResourceID != "access_live" {
		t.Fatal("CLI principal or admitted-scope request binding lost")
	}

}

func TestPeerNetworkStableErrors(t *testing.T) {
	for err, want := range map[error]int{peersessions.ErrDenied: http.StatusForbidden, peersessions.ErrConflict: http.StatusConflict, errors.New("database unavailable"): http.StatusServiceUnavailable} {
		w := httptest.NewRecorder()
		peerNetworkError(w, httptest.NewRequest(http.MethodPost, "/", nil), err)
		if w.Code != want {
			t.Fatalf("err=%v status=%d", err, w.Code)
		}
	}
}

func TestPeerNetworkRegistrationRejectsUnknownFields(t *testing.T) {
	fingerprint := hex.EncodeToString(bytes.Repeat([]byte{8}, 32))
	principalRequest := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/peer-network/register", strings.NewReader(body))
		return r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "account_1"}, Client: &auth.ClientPrincipal{SessionID: "cli_1"}}))
	}
	for name, body := range map[string]string{
		"unknown field": `{"operation_id":"operation_123","wireguard_public_key":"` + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)) + `","disco_public_key":"` + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) + `","expected_key_generation":0,"quic_certificate_fingerprint":"` + fingerprint + `","account_id":"attacker"}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &peerNetworkStub{}
			w := httptest.NewRecorder()
			peerNetworkRegister(stub).ServeHTTP(w, principalRequest(body))
			if w.Code != http.StatusBadRequest || stub.register.EndpointID != "" {
				t.Fatalf("status=%d registration=%+v body=%s", w.Code, stub.register, w.Body.String())
			}
		})
	}
}

// A machine that refreshes its configuration in a loop (2-6 requests a second per machine overloaded prod on
// 2026-10-26) is answered 429 with Retry-After before any proof or database work; another machine is unaffected.
func TestMachinePeerNetworkConfigIsRateLimitedPerMachineCredential(t *testing.T) {
	body := []byte(`{"operation_id":"operation_123"}`)
	verified := 0
	verifier := machineProofVerifierFunc(func(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
		verified++
		return controlplane.MachineRequestClaims{UserID: "account_1", MachineID: "machine_1", InstallationGeneration: 1, OperationID: "operation_123"}, nil
	})
	handler := machinePeerNetworkConfig(&peerNetworkStub{}, verifier, ratelimit.New(60, 2, 1000))
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/machine-peer-network/config", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := request("machine-a"); w.Code != http.StatusOK {
			t.Fatalf("request %d within the burst: status %d", i, w.Code)
		}
	}
	limited := request("machine-a")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" || verified != 2 {
		t.Fatalf("looping machine: status=%d retry-after=%q verified=%d, want 429 before verification", limited.Code, limited.Header().Get("Retry-After"), verified)
	}
	if w := request("machine-b"); w.Code != http.StatusOK {
		t.Fatalf("another machine was limited: status %d", w.Code)
	}
}
