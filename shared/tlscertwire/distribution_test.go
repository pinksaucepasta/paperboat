package tlscertwire

import (
	"encoding/hex"
	"testing"
	"time"
)

// The proof is an HMAC over json.Marshal, so field order and encodings are the wire. The expected MAC was
// computed from the JSON document spelled out independently of Go's encoder: reordering or retagging a
// field here breaks every edge's verification and fails this test first.
func TestProofMACIsByteStable(t *testing.T) {
	message := DistributionMessage{
		Version: 1, Action: "stage", CertificateID: "cert_1", AccountID: "acct_1", TunnelID: "tun_1", DomainID: "dom_1", TargetKind: "durable_route",
		RouteID: "route_1", Hostname: "app.example.test", DomainGeneration: 2, CertificateGeneration: 3, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_001",
		AssignmentGeneration: 4, Fingerprint: "ZmluZ2VycHJpbnQ", Issuer: "issuer", NotBefore: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC), CertificateExpiresAt: time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC),
		CertificatePEM: []byte("CERT"), PrivateKeyPEM: []byte("KEY"), IssuedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Proof: "ignored",
	}
	mac, err := ProofMAC(message, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil || hex.EncodeToString(mac) != "d92279921e45fb867606d840338c11ea01be2abd7503e9b57ce99d17ea03a52b" {
		t.Fatalf("proof mac = %x, %v", mac, err)
	}
}

func TestAckStatusMatchesAction(t *testing.T) {
	for action, status := range map[string]string{"stage": "ready", "activate": "active", "retire": "retired", "revoke": "revoked"} {
		if !AckStatusMatchesAction(action, status) || !AckStatusMatchesAction(action, "failed") || AckStatusMatchesAction(action, "bogus") {
			t.Errorf("%s/%s", action, status)
		}
	}
	if AckStatusMatchesAction("stage", "active") || AckStatusMatchesAction("other", "ready") {
		t.Error("mismatched status accepted")
	}
}
