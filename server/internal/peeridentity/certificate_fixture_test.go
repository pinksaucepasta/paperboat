package peeridentity

import (
	"crypto/ed25519"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"testing"
	"time"
)

func signedFixture(t *testing.T, private ed25519.PrivateKey, account string, role wire.Role, endpoint string, generation, serial uint64, issued, expires time.Time) []byte {
	t.Helper()
	value, err := wire.Sign(private, wire.Certificate{AccountID: account, Role: role, EndpointID: endpoint, QUICPublicKey: [32]byte{2}, Generation: generation, Serial: serial, IssuedAt: issued, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	return value.Raw
}
