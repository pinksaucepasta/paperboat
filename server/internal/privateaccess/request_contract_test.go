package privateaccess

import (
	"strings"
	"testing"
	"time"
)

func goldenRequest() Request {
	return Request{
		AccountID: "acct_1", ResourceKind: ResourcePreview, ResourceID: "prv_1", RouteID: "route_1", Audience: AudiencePreviewHTTP,
		MachineID: "machine_1", SessionID: "session_1", InstallationGeneration: 1, ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		Nonce: "nonce_1", OperationID: "op_1", CarrierSessionID: "carrier_1", RouteGeneration: 2, ProcessGeneration: 3, ConfigGeneration: 4,
		SessionGeneration: 5, AssignmentGeneration: 6, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001", Protocol: ProtocolHTTP,
		Method: "GET", Host: "app.example.test", Path: "/x", IdempotencyKey: "idem_1", RequestID: "req_1", CorrelationID: "cor_1",
	}
}

// The grant hash is signed over json.Marshal of the shared request, so the bytes must not move when the
// struct does. The constant is sha256 of the JSON document spelled out independently of Go's encoder.
func TestRequestHashIsByteStable(t *testing.T) {
	got, err := requestHash(goldenRequest())
	if err != nil || got != "46bdb986074c46a3a3691fde4acbfca6c88bda6aa591fd511fff964193b9e1f4" {
		t.Fatalf("request hash = %q, %v", got, err)
	}
}

// One identifier fence for the signed request: the edge verifies the same document with the same rule, so the
// server must not sign a grant the edge would refuse (it used to allow 256-byte idempotency keys, the edge 128).
func TestRequestUsesTheCarrierIdentifierFence(t *testing.T) {
	if err := validateRequest(goldenRequest()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Request){
		"idempotency key over 128 bytes": func(r *Request) { r.IdempotencyKey = strings.Repeat("k", 129) },
		"idempotency key with slash":     func(r *Request) { r.IdempotencyKey = "a/b" },
		"request id under 3 bytes":       func(r *Request) { r.RequestID = "ab" },
		"preview without operation id":   func(r *Request) { r.OperationID = "" },
		"preview with connector id":      func(r *Request) { r.ConnectorID = "connector_1" },
	} {
		r := goldenRequest()
		mutate(&r)
		if validateRequest(r) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
