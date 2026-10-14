package peeridentity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestApprovedEndpointCertificateVector(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/contracts/p2p-v1/fixtures/endpoint-certificate.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		RootPublicKey          string `json:"root_public_key"`
		KeyID                  string `json:"key_id"`
		Certificate            string `json:"certificate"`
		CertificateFingerprint string `json:"certificate_fingerprint"`
		Now                    string `json:"now"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	root, rootErr := base64.RawURLEncoding.Strict().DecodeString(vector.RootPublicKey)
	certificate, certificateErr := base64.RawURLEncoding.Strict().DecodeString(vector.Certificate)
	now, timeErr := time.Parse(time.RFC3339, vector.Now)
	if rootErr != nil || certificateErr != nil || timeErr != nil {
		t.Fatalf("root=%v certificate=%v time=%v", rootErr, certificateErr, timeErr)
	}
	verified, err := Verify(certificate, ed25519.PublicKey(root), Expected{AccountID: "account_01", Role: RoleCLI, EndpointID: "cli_01", Generation: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(root)
	if vector.KeyID != "aek_"+hex.EncodeToString(rootFingerprint[:]) || vector.CertificateFingerprint != hex.EncodeToString(verified.Fingerprint[:]) || verified.Serial != 7 {
		t.Fatalf("certificate vector identity mismatch")
	}
}

func TestVerifyCanonicalEndpointCertificate(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(nil)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	raw := signedFixture(t, rootPrivate, "account_01", RoleMachine, "machine_01", 3, 7, now.Add(-time.Minute), now.Add(time.Hour))
	certificate, err := Verify(raw, rootPublic, Expected{AccountID: "account_01", Role: RoleMachine, EndpointID: "machine_01", Generation: 3, Serial: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	if certificate.Fingerprint != sha256.Sum256(raw) || certificate.Role.String() != "machine" || len(certificate.Raw) != len(raw) {
		t.Fatalf("certificate=%+v", certificate)
	}
	raw[10] ^= 1
	if _, err := Verify(raw, rootPublic, Expected{}, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tamper error=%v", err)
	}
}

func TestVerifyRejectsIdentityValidityAndNonCanonicalBytes(t *testing.T) {
	rootPublic, rootPrivate, _ := ed25519.GenerateKey(nil)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	raw := signedFixture(t, rootPrivate, "account_01", RoleCLI, "cli_01", 1, 2, now.Add(-time.Minute), now.Add(time.Minute))
	if _, err := Verify(raw, rootPublic, Expected{EndpointID: "other"}, now); !errors.Is(err, ErrIdentity) {
		t.Fatalf("identity error=%v", err)
	}
	if _, err := Verify(raw, rootPublic, Expected{}, now.Add(time.Minute)); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("expiry error=%v", err)
	}
	for _, invalid := range [][]byte{nil, raw[:len(raw)-1], append(append([]byte(nil), raw...), 0)} {
		if _, err := Verify(invalid, rootPublic, Expected{}, now); err == nil {
			t.Fatalf("accepted invalid certificate length=%d", len(invalid))
		}
	}
}

func signedFixture(t *testing.T, root ed25519.PrivateKey, account string, role Role, endpoint string, generation, serial uint64, issued, expires time.Time) []byte {
	t.Helper()
	quic := [32]byte{2}
	payload := []byte{'P', 'B', 'E', 'C', 1}
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(account)))
	payload = append(payload, account...)
	payload = append(payload, byte(role))
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(endpoint)))
	payload = append(payload, endpoint...)
	payload = append(payload, quic[:]...)
	payload = binary.BigEndian.AppendUint64(payload, generation)
	payload = binary.BigEndian.AppendUint64(payload, serial)
	payload = binary.BigEndian.AppendUint64(payload, uint64(issued.Unix()))
	payload = binary.BigEndian.AppendUint64(payload, uint64(expires.Unix()))
	return append(payload, ed25519.Sign(root, payload)...)
}
