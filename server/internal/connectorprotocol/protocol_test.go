package connectorprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func testIdentity() (ed25519.PrivateKey, string, string) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	thumbprint, err := wire.IdentityThumbprint(public)
	if err != nil {
		panic(err)
	}
	keyID, err := wire.IdentityKeyID(public)
	if err != nil {
		panic(err)
	}
	return private, keyID, thumbprint
}

func testAuth(t *testing.T, now time.Time, generation uint64) (wire.AuthRequest, ed25519.PrivateKey) {
	t.Helper()
	private, keyID, thumbprint := testIdentity()
	request := wire.AuthRequest{AccountID: "acct_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", HostID: "host_1", IdentityKeyID: keyID, IdentityKeyThumbprint: thumbprint, ProcessGeneration: generation, CredentialGeneration: 4, Nonce: "nonce-1234567890", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	var err error
	request, err = wire.SignAuthProof(request, func(payload []byte) []byte { return ed25519.Sign(private, payload) })
	if err != nil {
		t.Fatal(err)
	}
	return request, private
}

func testConfigPayload(generation uint64, hostname string) []byte {
	return []byte(fmt.Sprintf(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":"tunnel_1","generation":%d,"name":"demo","desired_state":"active","access_mode":"public","stable_endpoint":"https://123e4567-e89b-42d3-a456-426614174000.tunnels.example.test","stable_endpoint_id":"endpoint_123e4567-e89b-42d3-a456-426614174000","expires_at":null,"routes":[{"id":"route_1","name":"default","protocol":"http","match_type":"exact","match_hostname":"%s","path_prefix":null,"origin_scheme":"http","origin_address":"127.0.0.1:3000","preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"ca_reference":null,"mtls_credential_reference":null,"connect_timeout_ms":10000,"idle_timeout_ms":90000,"max_concurrent_streams":128,"desired_state":"active"}]}`, generation, hostname))
}

func requiredCapabilityList() []string {
	return []string{wire.CapabilitySnapshot, wire.CapabilityDelta, wire.CapabilityAck, wire.CapabilityHeartbeat, wire.CapabilityRenewal, wire.CapabilityDrain}
}
