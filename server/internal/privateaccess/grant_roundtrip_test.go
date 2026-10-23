package privateaccess

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func TestMintGrantIssuerRoundTripsEveryAuthorizationFence(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	provider, err := mint.New([]mint.Key{{ID: "private-access-key", PrivateKey: ed25519.NewKeyFromSeed(seed)}}, "private-access-key", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewMintGrantIssuer(provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		AccountID: "account_1", ResourceKind: ResourcePreview, ResourceID: "preview_1", RouteID: "route_1",
		Audience: AudiencePreviewHTTP, MachineID: "machine_1", SessionID: "installation_7", InstallationGeneration: 7,
		ExpiresAt: now.Add(time.Minute), Nonce: "nonce_private_access_1", OperationID: "operation_preview_1",
		CarrierSessionID: "carrier_session_1", RouteGeneration: 2, SessionGeneration: 3, ProcessGeneration: 4,
		ConfigGeneration: 5, AssignmentGeneration: 6, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001",
		Protocol: ProtocolHTTP, Method: "CONNECT", Host: "private.example.test", Path: "/",
		IdempotencyKey: "private_access_idempotency_1", RequestID: "request_001", CorrelationID: "correlation_001",
	}
	token, err := issuer.MintGrant(context.Background(), request, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := issuer.VerifyGrant(context.Background(), token, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, request) {
		t.Fatalf("verified request = %#v, want %#v", got, request)
	}
}

type grantAttachmentStore struct{ attachment previewattachment.Attachment }

func (s grantAttachmentStore) Get(context.Context, string, string) (previewattachment.Attachment, error) {
	return s.attachment, nil
}

func TestPreviewAttachmentIssuesWireValidGrant(t *testing.T) {
	now := testClock()
	id := func(noun string) string { return noun + "_11111111-1111-4111-8111-111111111111" }
	r := testRequest(now)
	r.AccountID, r.ResourceID, r.RouteID = id("user"), id("preview"), id("route")
	r.MachineID, r.SessionID, r.CarrierSessionID = id("machine"), id("session"), "session_22222222-2222-4222-8222-222222222222"
	r.OperationID, r.IdempotencyKey, r.RequestID, r.CorrelationID = id("operation"), "operation_33333333-3333-4333-8333-333333333333", id("request"), id("correlation")
	r.AssignmentGeneration = r.SessionGeneration
	a := previewattachment.Attachment{
		Binding: previewattachment.Binding{AccountID: r.AccountID, PreviewID: r.ResourceID, OperationID: r.OperationID,
			OwnerMachineID: r.MachineID, OwnerSessionID: id("session"), TunnelID: r.MachineID, ConnectorID: r.MachineID,
			SessionID: r.CarrierSessionID, RouteID: r.RouteID, LeaseGeneration: r.SessionGeneration,
			RouteGeneration: r.RouteGeneration, ProcessGeneration: r.ProcessGeneration, ConfigGeneration: r.ConfigGeneration,
			EdgeNodeID: r.EdgeNodeID, EdgeProcessEpoch: r.EdgeProcessEpoch},
		Endpoint: "https://" + r.Host, AccessMode: "private", State: previewattachment.StateReady, ExpiresAt: now.Add(time.Minute),
	}
	resolver, err := NewPreviewAttachmentResolver(grantAttachmentStore{a})
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	provider, err := mint.New([]mint.Key{{ID: "preview-grant", PrivateKey: ed25519.NewKeyFromSeed(seed)}}, "preview-grant", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewMintGrantIssuer(provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(resolver, nil, Config{Now: testClock, GrantMinter: issuer, GrantVerifier: issuer, MachineState: &fakeMachineState{}})
	if err != nil {
		t.Fatal(err)
	}
	edge := EdgeIdentity{NodeID: r.EdgeNodeID, ProcessEpoch: r.EdgeProcessEpoch}
	// The HTTP boundary receives a fractional expiry in a non-UTC zone.
	r.ExpiresAt = r.ExpiresAt.Add(123 * time.Millisecond).In(time.FixedZone("client", 3600))
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var issue GrantIssueRequest
	if err = json.Unmarshal(encoded, &issue); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	machine := &fakeMachineRequest{claims: controlplane.MachineRequestClaims{UserID: r.AccountID, MachineID: r.MachineID, InstallationGeneration: int64(r.InstallationGeneration), SessionGeneration: int64(r.SessionGeneration), CredentialJTI: r.SessionID, OperationID: r.IdempotencyKey}}
	handler, err := NewGrantHTTPHandler(service, machine)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, GrantPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer machine_control_token")
	req.Header.Set("X-Paperboat-Machine-Identity", "machine_control_token")
	req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status/body=%d/%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(machine.body, body) {
		t.Fatal("proof verification did not use exact original request bytes")
	}
	var response GrantIssueResponse
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	r.ExpiresAt = r.ExpiresAt.UTC().Truncate(time.Second)
	if response.ExpiresAt != r.ExpiresAt || response.Request != r {
		t.Fatal("response did not return canonical signed request expiry")
	}
	token := response.Grant
	signed, err := issuer.VerifyGrant(context.Background(), token, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	var wire connectorprotocol.PrivateAccessRequest
	if err = json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if err = wire.Validate(now); err != nil {
		t.Fatalf("signed preview wire: %v", err)
	}
	if signed != r {
		t.Fatal("signed grant changed exact preview binding")
	}
	_, err = service.Authorize(context.Background(), AuthorizeInput{Edge: edge, Request: signed, Grant: token})
	if err != nil {
		t.Fatalf("authorize actual preview attachment grant: %v", err)
	}
}
