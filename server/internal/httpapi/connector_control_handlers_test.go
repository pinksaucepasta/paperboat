package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
)

type bootstrapSessionReader struct {
	value connectorprotocol.ActiveControlSession
	ok    bool
}

func (r bootstrapSessionReader) Lookup(tunnelID, connectorID, sessionID string, generation uint64) (connectorprotocol.ActiveControlSession, bool) {
	if !r.ok || tunnelID != r.value.TunnelID || connectorID != r.value.ConnectorID || sessionID != r.value.SessionID || generation != r.value.ProcessGeneration {
		return connectorprotocol.ActiveControlSession{}, false
	}
	return r.value, true
}

type bootstrapSource struct {
	value connectorprotocol.CarrierBootstrapDescriptor
	err   error
}

func (s bootstrapSource) Descriptor(context.Context, connectorprotocol.ActiveControlSession) (connectorprotocol.CarrierBootstrapDescriptor, error) {
	return s.value, s.err
}

type bootstrapVerifier struct {
	claims controlplane.MachineRequestClaims
}

func (v bootstrapVerifier) VerifyMachineRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
	return v.claims, nil
}

func TestConnectorCarrierBootstrapRequiresExactMachineAndSessionBinding(t *testing.T) {
	now := time.Now().UTC()
	active := connectorprotocol.ActiveControlSession{
		AccountID: "account_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", HostID: "host_01",
		SessionID: "session_01", IdentityKeyID: "ed25519:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		IdentityKeyThumbprint: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ProcessGeneration: 2,
		CredentialGeneration: 3, ConfigGeneration: 4,
		ConfigContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	const stableEndpointID = "endpoint_11111111-1111-4111-8111-111111111111"
	descriptor := connectorprotocol.CarrierBootstrapDescriptor{
		Schema: connectorprotocol.CarrierBootstrapSchema, Kind: "carrier_bootstrap_descriptor",
		AccountID: active.AccountID, TunnelID: active.TunnelID, ConnectorID: active.ConnectorID, HostID: active.HostID,
		StableEndpointID: stableEndpointID,
		SessionID:        active.SessionID, ProcessGeneration: active.ProcessGeneration, CredentialGeneration: active.CredentialGeneration,
		ConfigGeneration: active.ConfigGeneration, ConfigContentHash: active.ConfigContentHash,
		Carriers: []connectorprotocol.CarrierBootstrapNode{{
			EdgeNodeID: "edge_01", EdgeProcessEpoch: "edge_epoch_01", FailureDomain: "region_a",
			Endpoints:                 []string{"h2://edge.example.test:8443", "h3://edge.example.test:8444"},
			ServerSPKISHA256:          "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ServerCertificateChainPEM: bootstrapTestCertificate(t),
		}}, IssuedAt: now, ExpiresAt: now.Add(10 * time.Second),
	}
	handler := connectorCarrierBootstrap(bootstrapSessionReader{value: active, ok: true}, bootstrapSource{value: descriptor}, bootstrapVerifier{claims: controlplane.MachineRequestClaims{MachineID: active.HostID, UserID: active.AccountID, OperationID: "bootstrap_request_01", InstallationGeneration: 1}}, notGone)
	body, _ := json.Marshal(connectorprotocol.CarrierBootstrapRequest{Schema: connectorprotocol.CarrierBootstrapSchema, Kind: "carrier_bootstrap_request", SessionID: active.SessionID, ProcessGeneration: active.ProcessGeneration, ConfigGeneration: active.ConfigGeneration, ConfigContentHash: active.ConfigContentHash})
	request := httptest.NewRequest(http.MethodPost, "/v1/tunnels/tunnel_01/connectors/connector_01/carrier-bootstrap", strings.NewReader(string(body)))
	request.SetPathValue("tunnel_id", active.TunnelID)
	request.SetPathValue("connector_id", active.ConnectorID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Paperboat-Machine-Identity", strings.Repeat("m", 48))
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	request.Header.Set("Idempotency-Key", "bootstrap_request_01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"session_id":"session_01"`) || !strings.Contains(response.Body.String(), `"stable_endpoint_id":"`+stableEndpointID+`"`) || strings.Contains(response.Body.String(), "private_key") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	// An applied replacement does not revoke the exact still-serving generation.
	candidate := active
	candidate.ConfigGeneration = 5
	candidate.ConfigContentHash = "sha256:" + strings.Repeat("b", 64)
	candidate.ServingConfigGeneration, candidate.ServingConfigContentHash = active.ConfigGeneration, active.ConfigContentHash
	lkgHandler := connectorCarrierBootstrap(bootstrapSessionReader{value: candidate, ok: true}, bootstrapSource{value: descriptor}, bootstrapVerifier{claims: controlplane.MachineRequestClaims{MachineID: active.HostID, UserID: active.AccountID, OperationID: "bootstrap_request_01", InstallationGeneration: 1}}, notGone)
	lkgRequest := request.Clone(request.Context())
	lkgRequest.Body = io.NopCloser(strings.NewReader(string(body)))
	lkgResponse := httptest.NewRecorder()
	lkgHandler.ServeHTTP(lkgResponse, lkgRequest)
	if lkgResponse.Code != http.StatusOK {
		t.Fatalf("serving generation denied while candidate applied: %d", lkgResponse.Code)
	}

	stale := httptest.NewRequest(http.MethodPost, request.URL.Path, strings.NewReader(strings.Replace(string(body), `"config_generation":4`, `"config_generation":5`, 1)))
	stale.SetPathValue("tunnel_id", active.TunnelID)
	stale.SetPathValue("connector_id", active.ConnectorID)
	stale.Header = request.Header.Clone()
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusConflict || !strings.Contains(staleResponse.Body.String(), "connector_session_stale") {
		t.Fatalf("stale status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}

	// A config edit within the session: the connector acknowledged generation 5 and bootstraps its
	// carrier for it before reporting ready. That is the same session, not a stale one (it used to
	// answer connector_session_stale, and the connector's reconnect re-generated the tunnel's route
	// on every edge, cutting transfers in flight).
	editing := active
	editing.ServingConfigGeneration, editing.ServingConfigContentHash = active.ConfigGeneration, active.ConfigContentHash
	editing.CandidateConfigGeneration, editing.CandidateConfigContentHash = 5, "sha256:"+strings.Repeat("b", 64)
	editedDescriptor := descriptor
	editedDescriptor.ConfigGeneration, editedDescriptor.ConfigContentHash = editing.CandidateConfigGeneration, editing.CandidateConfigContentHash
	editHandler := connectorCarrierBootstrap(bootstrapSessionReader{value: editing, ok: true}, bootstrapSource{value: editedDescriptor}, bootstrapVerifier{claims: controlplane.MachineRequestClaims{MachineID: active.HostID, UserID: active.AccountID, OperationID: "bootstrap_request_01", InstallationGeneration: 1}}, notGone)
	editBody := strings.Replace(strings.Replace(string(body), `"config_generation":4`, `"config_generation":5`, 1), active.ConfigContentHash, editing.CandidateConfigContentHash, 1)
	edit := httptest.NewRequest(http.MethodPost, request.URL.Path, strings.NewReader(editBody))
	edit.SetPathValue("tunnel_id", active.TunnelID)
	edit.SetPathValue("connector_id", active.ConnectorID)
	edit.Header = request.Header.Clone()
	editResponse := httptest.NewRecorder()
	editHandler.ServeHTTP(editResponse, edit)
	if editResponse.Code != http.StatusOK || !strings.Contains(editResponse.Body.String(), `"config_generation":5`) {
		t.Fatalf("candidate config bootstrap status=%d body=%s", editResponse.Code, editResponse.Body.String())
	}

	// A connector whose tunnel expired (or was deleted, or whose machine was revoked) is told it is
	// gone instead of retrying a 503 forever; the carrier source is never asked.
	goneHandler := connectorCarrierBootstrap(bootstrapSessionReader{value: active, ok: true}, bootstrapSource{err: errors.New("descriptor must not be read")}, bootstrapVerifier{claims: controlplane.MachineRequestClaims{MachineID: active.HostID, UserID: active.AccountID, OperationID: "bootstrap_request_01", InstallationGeneration: 1}}, func(_ context.Context, tunnelID, connectorID string) (bool, error) {
		return tunnelID == active.TunnelID && connectorID == active.ConnectorID, nil
	})
	goneRequest := request.Clone(request.Context())
	goneRequest.Body = io.NopCloser(strings.NewReader(string(body)))
	goneResponse := httptest.NewRecorder()
	goneHandler.ServeHTTP(goneResponse, goneRequest)
	var goneBody struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if goneResponse.Code != http.StatusGone || json.Unmarshal(goneResponse.Body.Bytes(), &goneBody) != nil || goneBody.Error.Code != "connector_gone" || goneBody.Error.Retryable {
		t.Fatalf("gone connector status=%d body=%s", goneResponse.Code, goneResponse.Body.String())
	}
}

func notGone(context.Context, string, string) (bool, error) { return false, nil }

func bootstrapTestCertificate(t *testing.T) string {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "edge.example.test"}, DNSNames: []string{"edge.example.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}))
}
