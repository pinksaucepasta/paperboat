package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

func TestTemporaryBrowserIdentityIsSelfSignedAndFingerprintBindsSPKI(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	identity, err := newBrowserIdentity(now)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(identity.PrivateKey)
	if len(identity.TLS.Certificate) != 1 || identity.TLS.Leaf == nil || identity.TLS.Leaf.CheckSignature(identity.TLS.Leaf.SignatureAlgorithm, identity.TLS.Leaf.RawTBSCertificate, identity.TLS.Leaf.Signature) != nil {
		t.Fatal("browser identity is not one self-signed leaf")
	}
	if _, ok := identity.TLS.Leaf.PublicKey.(ed25519.PublicKey); !ok || !identity.TLS.Leaf.NotBefore.Before(now) || !now.Before(identity.TLS.Leaf.NotAfter) {
		t.Fatal("browser identity does not have a currently valid Ed25519 leaf")
	}
	if identity.TLS.Leaf.NotAfter.Sub(identity.TLS.Leaf.NotBefore) > 10*time.Minute {
		t.Fatalf("certificate lifetime = %s", identity.TLS.Leaf.NotAfter.Sub(identity.TLS.Leaf.NotBefore))
	}
	if len(identity.TLS.Leaf.ExtKeyUsage) != 1 || identity.TLS.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || identity.TLS.Leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Fatal("browser identity is missing the required client key usage")
	}
	spki, err := x509.MarshalPKIXPublicKey(identity.TLS.Leaf.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(identity.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Sum(spki)
	if !bytes.Equal(got, want) {
		t.Fatal("ticket fingerprint does not match the browser certificate SPKI")
	}
}

func TestVerifyIdentityEnvelopeRequiresImportedRootAndExactMachine(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpointCertificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{
		AccountID:     "account_owner_1",
		Role:          endpointidentity.RoleMachine,
		EndpointID:    "machine_1",
		QUICPublicKey: machinePrivate.Public().(ed25519.PublicKey),
		Generation:    3,
		Serial:        1,
		IssuedAt:      now.Add(-time.Minute),
		ExpiresAt:     now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := endpointCertificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(identityEnvelope{Certificate: base64.RawURLEncoding.EncodeToString(raw), RootKeyID: accountRootKeyID(rootPublic)})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifyIdentityEnvelope(envelope, rootPublic, accountRootKeyID(rootPublic), "account_owner_1", "machine_1", now)
	if err != nil || verified.Certificate.Claims.EndpointID != "machine_1" {
		t.Fatalf("valid envelope rejected: identity=%+v err=%v", verified.Certificate.Claims, err)
	}
	if _, err := verifyIdentityEnvelope(envelope, rootPublic, accountRootKeyID(rootPublic), "account_other_1", "machine_1", now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("wrong account accepted: %v", err)
	}
	if _, err := verifyIdentityEnvelope(envelope, rootPublic, accountRootKeyID(rootPublic), "account_owner_1", "machine_other_1", now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("wrong machine accepted: %v", err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyIdentityEnvelope(envelope, otherPublic, accountRootKeyID(otherPublic), "account_owner_1", "machine_1", now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("untrusted root accepted: %v", err)
	}
	if _, err := verifyIdentityEnvelope(envelope, rootPublic, "aek_wrong", "account_owner_1", "machine_1", now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("unmatched root key ID accepted: %v", err)
	}
	unknown, _ := json.Marshal(map[string]string{"certificate": base64.RawURLEncoding.EncodeToString(raw), "root_key_id": accountRootKeyID(rootPublic), "root_public_key": base64.RawURLEncoding.EncodeToString(rootPublic)})
	if _, err := verifyIdentityEnvelope(unknown, rootPublic, accountRootKeyID(rootPublic), "account_owner_1", "machine_1", now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("server-supplied root field accepted: %v", err)
	}
}

func TestMachineTLSLeafMustMatchRootSignedEndpointKeyAndALPN(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpointCertificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: "account_owner_1", Role: endpointidentity.RoleMachine, EndpointID: "machine_1", QUICPublicKey: machinePublic, Generation: 3, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := endpointCertificate.MarshalBinary()
	verified, err := endpointidentity.Verify(raw, rootPublic, endpointidentity.Expected{AccountID: "account_owner_1", Role: endpointidentity.RoleMachine, EndpointID: "machine_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	peer := machineIdentity{Certificate: verified, RootPublic: rootPublic, Raw: raw}
	template := &x509.Certificate{SerialNumber: bigOne(), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, machinePublic, machinePrivate)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, NegotiatedProtocol: innerALPN}
	if err := verifyMachineTLSLeaf(state, peer, now); err != nil {
		t.Fatalf("valid machine leaf rejected: %v", err)
	}
	state.NegotiatedProtocol = "wrong"
	if err := verifyMachineTLSLeaf(state, peer, now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("wrong ALPN accepted: %v", err)
	}
	state.NegotiatedProtocol = innerALPN
	_, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPublic := otherPrivate.Public().(ed25519.PublicKey)
	otherDER, err := x509.CreateCertificate(rand.Reader, template, template, otherPublic, otherPrivate)
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, _ := x509.ParseCertificate(otherDER)
	state.PeerCertificates = []*x509.Certificate{otherLeaf}
	if err := verifyMachineTLSLeaf(state, peer, now); !errors.Is(err, errInvalidPeerIdentity) {
		t.Fatalf("TLS leaf with different key accepted: %v", err)
	}
}

type countedFrameWriter struct {
	bytes.Buffer
	writes int
}

func (w *countedFrameWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(data)
}

func TestApplicationFrameUsesOneWriteForHeaderAndPayload(t *testing.T) {
	var writer countedFrameWriter
	payload := []byte("terminal input")
	if err := writeApplicationFrame(&writer, appKindBinary, payload); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("application frame used %d TLS writes", writer.writes)
	}
	kind, got, err := readApplicationFrame(&writer)
	if err != nil || kind != appKindBinary || !bytes.Equal(got, payload) {
		t.Fatalf("combined frame did not round trip: kind=%d err=%v", kind, err)
	}
}

func TestApplicationFramesAreBoundedAndIndependentOfReadChunking(t *testing.T) {
	var wire bytes.Buffer
	structured := []byte(`{"type":"hello"}`)
	binaryFrame := []byte{protocol.TerminalOutputOpcode, 1, 0}
	if err := writeApplicationFrame(&wire, appKindStructured, structured); err != nil {
		t.Fatal(err)
	}
	if err := writeApplicationFrame(&wire, appKindBinary, binaryFrame); err != nil {
		t.Fatal(err)
	}
	reader := chunkedReader{reader: bytes.NewReader(wire.Bytes()), chunk: 2}
	kind, got, err := readApplicationFrame(reader)
	if err != nil || kind != appKindStructured || !bytes.Equal(got, structured) {
		t.Fatalf("structured frame = (%d, %q, %v)", kind, got, err)
	}
	kind, got, err = readApplicationFrame(reader)
	if err != nil || kind != appKindBinary || !bytes.Equal(got, binaryFrame) {
		t.Fatalf("binary frame = (%d, %v, %v)", kind, got, err)
	}
	oversized := make([]byte, 5)
	oversized[0] = appKindStructured
	binary.BigEndian.PutUint32(oversized[1:], appStructuredMax+1)
	if _, _, err := readApplicationFrame(bytes.NewReader(oversized)); err == nil {
		t.Fatal("oversized structured frame accepted")
	}
	if err := writeApplicationFrame(io.Discard, appKindBinary, make([]byte, appBinaryMax+1)); err == nil {
		t.Fatal("oversized binary frame accepted")
	}
}

type chunkedReader struct {
	reader io.Reader
	chunk  int
}

func (r chunkedReader) Read(target []byte) (int, error) {
	if len(target) > r.chunk {
		target = target[:r.chunk]
	}
	return r.reader.Read(target)
}

func sha256Sum(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

func bigOne() *big.Int { return big.NewInt(1) }
