package connectorprotocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func testAdmission(t *testing.T, now time.Time) PrivateAccessAdmission {
	t.Helper()
	public := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	thumbprint, err := IdentityThumbprint(public)
	if err != nil {
		t.Fatal(err)
	}
	return PrivateAccessAdmission{
		Schema: PrivateAccessSchema, Kind: PrivateAccessAdmissionKind, AccountID: "acct_1", MachineID: "machine_1", InstallationGeneration: 1,
		AccessorPublicKey: base64.RawURLEncoding.EncodeToString(public), AccessorThumbprint: thumbprint, ResourceKind: "preview", ResourceID: "prv_1",
		OperationID: "op_1", CarrierSessionID: "carrier_1", RouteID: "route_1", RouteGeneration: 1, SessionGeneration: 1, ProcessGeneration: 1,
		ConfigGeneration: 1, AssignmentGeneration: 1, AssignmentID: "assign_1", ConfigContentHash: "sha256:" + strings.Repeat("a", 64),
		EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001", Protocol: "http", Hostname: "app.example.test", MatchType: "exact",
		EdgeEndpoints: []string{"h2://edge.example.test:25001", "h3://edge.example.test:25002"}, ExpiresAt: now.Add(time.Minute), TunnelID: "tun_1",
		CarrierConnectorID: "connector_1", EdgeCarrierServerSPKISHA256: "sha256:" + strings.Repeat("b", 64), EdgeCarrierServerCertificateChainPEM: "chain",
	}
}

func TestPrivateAccessAdmissionValidate(t *testing.T) {
	now := time.Now().UTC()
	valid := testAdmission(t, now)
	if err := valid.Validate(now); err != nil {
		t.Fatal(err)
	}
	// The server admits a private HTTP catch-all route under its stable hostname: every side accepts it.
	catchAll := valid
	catchAll.MatchType = "catch_all"
	if err := catchAll.Validate(now); err != nil {
		t.Fatalf("HTTP catch_all: %v", err)
	}
	for name, mutate := range map[string]func(*PrivateAccessAdmission){
		"tls endpoint":         func(a *PrivateAccessAdmission) { a.EdgeEndpoints[0] = "tls://edge.example.test:25001" },
		"zero port":            func(a *PrivateAccessAdmission) { a.EdgeEndpoints[1] = "h3://edge.example.test:0" },
		"preview no operation": func(a *PrivateAccessAdmission) { a.OperationID = "" },
		"thumbprint mismatch":  func(a *PrivateAccessAdmission) { a.AccessorThumbprint = strings.Repeat("A", 43) },
		"uppercase hash":       func(a *PrivateAccessAdmission) { a.ConfigContentHash = "sha256:" + strings.Repeat("A", 64) },
		"expired":              func(a *PrivateAccessAdmission) { a.ExpiresAt = now },
		"unknown match":        func(a *PrivateAccessAdmission) { a.MatchType = "recursive" },
		"private tcp host":     func(a *PrivateAccessAdmission) { a.Protocol = "private_tcp" },
		"tunnel no names":      func(a *PrivateAccessAdmission) { a.ResourceKind = "tunnel"; a.OperationID = ""; a.ConnectorID = "c_1" },
		"unknown kind":         func(a *PrivateAccessAdmission) { a.ResourceKind = "other" },
	} {
		a := valid
		a.EdgeEndpoints = append([]string(nil), valid.EdgeEndpoints...)
		mutate(&a)
		if a.Validate(now) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPrivateAccessRequestIdentifierFence(t *testing.T) {
	r := PrivateAccessRequest{
		AccountID: "acct_1", ResourceKind: "preview", ResourceID: "prv_1", RouteID: "route_1", Audience: "paperboat-preview-http", MachineID: "machine_1",
		SessionID: "session_1", InstallationGeneration: 1, ExpiresAt: time.Now().Add(time.Minute), Nonce: "nonce_1", OperationID: "op_1",
		CarrierSessionID: "carrier_1", RouteGeneration: 1, ProcessGeneration: 1, ConfigGeneration: 1, SessionGeneration: 1, AssignmentGeneration: 1,
		EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001", Protocol: "http", Method: "GET", Host: "app.example.test", Path: "/",
		IdempotencyKey: "idem_1", RequestID: "req_1", CorrelationID: "cor_1",
	}
	if err := r.Validate(time.Time{}); err != nil {
		t.Fatal(err)
	}
	r.IdempotencyKey = strings.Repeat("k", MaxIdentifierBytes+1)
	if r.Validate(time.Time{}) == nil {
		t.Fatal("idempotency key over the identifier limit accepted")
	}
	r.IdempotencyKey, r.RequestID = "idem_1", "ab"
	if r.Validate(time.Time{}) == nil {
		t.Fatal("request id under 3 bytes accepted")
	}
}
